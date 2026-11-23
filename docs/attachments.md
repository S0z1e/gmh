# Text file attachments

Attach a UTF-8 text file such as a `.log`, `.txt`, `.json`, or `.csv` by
content. File extensions are optional.

- In the REPL or TUI, enter `/attach server.log` before your next task.
  Enter `/attach` to list queued text files.
- From the CLI, use `harness -attach server.log -p "Summarize these errors"`.
  Repeat `-attach` to include multiple files on the first turn.
- From a GUI, send the [`attach_file` RPC request](rpc.md) before `prompt`.
  Use `list_pending_files` to list queued text files.

Relative paths resolve against the workspace. You can also supply an absolute
path, including a file outside the workspace. Each turn accepts up to ten text
files totaling 64 KiB, subject to the model's input budget. If a task and its
queued attachments exceed that budget, the files stay queued so you can retry
with a shorter prompt or a larger context window. Missing files, directories,
binary files, and files over the limit return an error.

The harness reads each file when you attach it and sends the snapshot as text
with your next task. The session transcript stores your original task and a
`files` list with each attachment's path and contents. Later turns and resumed
sessions use the saved snapshot. As with other context, compaction can summarize
earlier attachments. Use [`/image`](images.md) to attach an image.
