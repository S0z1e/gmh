package tools

import (
	"strings"
)

// DeniedCommand checks a bash command against the denylist and returns a
// human-readable reason when it is refused, or "" when it is allowed.
//
// This is harm reduction, not a sandbox: it is plain pattern matching
// over the command text, so it can be bypassed (variables, quoting,
// indirection through another binary). It exists to catch the obvious,
// catastrophic mistakes before they run — not to make bash safe. See the
// README's safety section for what suggest-mode can and cannot promise.
func DeniedCommand(cmd string) string {
	lower := strings.ToLower(cmd)
	// Nested shell syntax may execute writes inside a read-only command.
	// Keep the old conservative denial across the whole command, including
	// separators inside substitutions. ${HOME} itself is just a pathname.
	nested := strings.ContainsAny(cmd, "()`") || strings.Contains(strings.ReplaceAll(lower, "${home}", ""), "${")
	if nested && strings.Contains(cmd, ">") && credentialPath(cmd) {
		return "nested shell command may write into a credential directory"
	}

	// Read-only commands are checked only at output destinations, so reading
	// credentials and redirecting stderr elsewhere is allowed. For write
	// commands retain the conservative check of every credential reference:
	// options and auxiliary outputs need not be interpreted as shell syntax.
	for _, seg := range splitSegments(cmd) {
		fields := shellWords(seg)
		for i, field := range fields {
			if field == ">" && i+1 < len(fields) && credentialPath(fields[i+1]) {
				return "command redirects output into a credential directory"
			}
		}
		fields = unwrapCommand(withoutRedirects(fields))
		if len(fields) < 2 {
			continue
		}
		if credentialPath(seg) && strings.Contains(seg, ">") {
			// Do not interpret nested shell commands or arbitrary interpreters
			// as simple reads just because their outer redirect looks harmless.
			simpleRead := false
			switch fields[0] {
			case "ls", "cat", "head", "tail", "grep", "wc", "echo", "printf":
				simpleRead = !strings.Contains(seg, "$(") && !strings.Contains(seg, "`")
			}
			if !simpleRead {
				return "command may redirect output into a credential directory"
			}
		}
		switch fields[0] {
		case "cp", "mv", "install", "ln", "rsync", "scp":
			if credentialPath(seg) {
				return "command writes into a credential directory"
			}
		case "tee", "touch", "mkdir", "chmod", "chown":
			for _, f := range fields[1:] {
				if credentialPath(f) {
					return "command writes into a credential directory"
				}
			}
		}
	}

	// The Docker socket: mounting it is effectively root on the host.
	if strings.Contains(lower, "docker.sock") {
		return "command touches the Docker socket (/var/run/docker.sock)"
	}

	// Keep the original conservative split for write commands and catastrophic
	// operations, including separators inside quoted interpreter strings.
	// The quote-aware split above is only for locating redirect targets.
	for _, seg := range strings.FieldsFunc(lower, func(r rune) bool {
		return r == ';' || r == '&' || r == '|' || r == '\n'
	}) {
		// Preserve HEAD's credential-reference check even when an inner write
		// is hidden inside a quoted shell command or command substitution.
		raw := unwrapCommand(strings.Fields(seg))
		if credentialPath(lower) && len(raw) > 0 {
			switch raw[0] {
			case "cp", "mv", "tee", "install", "ln", "touch", "mkdir", "chmod", "chown", "rsync", "scp":
				return "command writes into a credential directory"
			}
		}
		fields := unwrapCommand(shellWords(seg))
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "rm":
			if deniesRm(fields) {
				return "rm -rf against / (or the home directory) is denied"
			}
		case "mkfs", "mkfs.ext4", "mkfs.xfs", "mkfs.btrfs", "mkfs.vfat":
			// --help / --version alone is harmless; a device argument
			// means it is about to format something.
			for _, f := range fields[1:] {
				if !strings.HasPrefix(f, "-") {
					return "mkfs (formatting a filesystem) is denied"
				}
			}
		case "dd":
			for _, f := range fields[1:] {
				if strings.HasPrefix(f, "of=/dev/") {
					return "dd writing to a device is denied"
				}
			}
		}
	}
	return ""
}

// splitSegments splits a command on ; & | and newlines. It is a rough
// lexical split, not a shell parser — good enough for a denylist.
func splitSegments(cmd string) []string {
	var segments []string
	var quote byte
	start := 0
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if c == '\\' && quote != '\'' && i+1 < len(cmd) {
			i++
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c == '>' {
			for i+1 < len(cmd) && strings.ContainsRune(">|&", rune(cmd[i+1])) {
				i++
			}
			continue
		}
		if c == '&' && i+1 < len(cmd) && cmd[i+1] == '>' {
			continue
		}
		if c == ';' || c == '&' || c == '|' || c == '\n' {
			segments = append(segments, cmd[start:i])
			start = i + 1
		}
	}
	return append(segments, cmd[start:])
}

func credentialPath(path string) bool {
	path = strings.ToLower(path)
	for _, base := range []string{"~", "$home", "${home}"} {
		for _, dir := range []string{"/.ssh", "/.aws", "/.config/gcloud", "/.kube", "/.docker"} {
			prefix := base + dir
			if strings.Contains(path, prefix) {
				return true
			}
		}
	}
	return false
}

func withoutRedirects(fields []string) []string {
	var out []string
	for i := 0; i < len(fields); i++ {
		if fields[i] == ">" {
			if len(out) > 0 {
				numeric := true
				for _, c := range out[len(out)-1] {
					if c < '0' || c > '9' {
						numeric = false
					}
				}
				if numeric {
					out = out[:len(out)-1]
				}
			}
			for i+1 < len(fields) && fields[i+1] == ">" {
				i++
			}
			if i+1 < len(fields) {
				i++
			}
			continue
		}
		out = append(out, fields[i])
	}
	return out
}

// shellWords is a lexical pass, not an evaluator or shell sandbox.
func shellWords(s string) []string {
	// Classify combined/clobber redirects like ordinary output redirects.
	// The original command is never rewritten for execution.
	s = strings.NewReplacer(">|", ">", ">&", ">", "&>", ">").Replace(s)
	var words []string
	var word strings.Builder
	var quote rune
	flush := func() {
		if word.Len() > 0 {
			words = append(words, word.String())
			word.Reset()
		}
	}
	for _, r := range s {
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
		case ' ', '\t':
			flush()
		case '>':
			flush()
			words = append(words, ">")
		default:
			word.WriteRune(r)
		}
	}
	flush()
	return words
}

func unwrapCommand(fields []string) []string {
	for len(fields) > 0 {
		name := fields[0]
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		fields[0] = name
		switch name {
		case "sudo", "command", "exec", "env", "nohup", "doas":
			fields = fields[1:]
			for len(fields) > 0 && (strings.HasPrefix(fields[0], "-") || strings.Contains(fields[0], "=")) {
				if (fields[0] == "-u" || fields[0] == "-g") && len(fields) > 1 {
					fields = fields[1:]
				}
				fields = fields[1:]
			}
		default:
			return fields
		}
	}
	return fields
}

// deniesRm reports whether an rm invocation is a recursive-force delete
// of the filesystem root or the home directory. Flags may be combined
// (-rf) or separate (-r -f).
func deniesRm(fields []string) bool {
	recursive, force := false, false
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "-") && f != "-" {
			if strings.HasPrefix(f, "--") {
				switch f {
				case "--recursive":
					recursive = true
				case "--force":
					force = true
				}
			} else {
				if strings.ContainsAny(f, "rR") {
					recursive = true
				}
				if strings.Contains(f, "f") {
					force = true
				}
			}
			continue
		}
		if recursive && force {
			switch strings.TrimRight(f, "/") {
			case "", "/*", "~", "$home", "${home}":
				return true
			}
		}
	}
	return false
}
