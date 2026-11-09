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

	// Credential directories: never let the agent WRITE into them.
	// (The research lists ~/.ssh, ~/.aws, ~/.config/gcloud, ~/.kube,
	// ~/.docker. $HOME forms are the same directories spelled out.)
	// Reading them (cat, ls, grep) is allowed — only a write indicator
	// (a redirect, or a copy/move/write command) triggers the denial,
	// so grepping for the path itself still works.
	for _, dir := range []string{
		"~/.ssh", "~/.aws", "~/.config/gcloud", "~/.kube", "~/.docker",
		"$home/.ssh", "$home/.aws", "$home/.config/gcloud", "$home/.kube", "$home/.docker",
		"${home}/.ssh", "${home}/.aws", "${home}/.config/gcloud", "${home}/.kube", "${home}/.docker",
	} {
		if strings.Contains(lower, dir) && hasWriteIndicator(lower) {
			return "command writes into a credential directory (" + dir + "); use a repo-scoped, short-lived token instead"
		}
	}

	// The Docker socket: mounting it is effectively root on the host.
	if strings.Contains(lower, "docker.sock") {
		return "command touches the Docker socket (/var/run/docker.sock)"
	}

	// Split on shell separators so one bad segment cannot hide behind a
	// harmless first command.
	for _, seg := range splitSegments(lower) {
		fields := strings.Fields(seg)
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
	return strings.FieldsFunc(cmd, func(r rune) bool {
		return r == ';' || r == '&' || r == '|' || r == '\n'
	})
}

// hasWriteIndicator reports whether the command writes somewhere: a
// redirect, or a copy/move/write command.
func hasWriteIndicator(lower string) bool {
	if strings.Contains(lower, ">") {
		return true
	}
	for _, seg := range splitSegments(lower) {
		fields := strings.Fields(seg)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "cp", "mv", "tee", "install", "ln", "touch", "mkdir", "chmod", "chown", "rsync", "scp":
			return true
		}
	}
	return false
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
			switch f {
			case "/", "/*", "~", "$home", "${home}":
				return true
			}
		}
	}
	return false
}
