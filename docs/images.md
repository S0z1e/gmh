# Images

The harness can send images to a model that declares image input
(`"input": ["text", "image"]` in the config). Three ways in:
- `-image <path>` (repeatable) attaches images to the first turn,
  in `-p` or the REPL.
- `/image <path>` in the REPL attaches to your next message
  (`/image` alone lists what is attached).
- The model itself can `read` an image file (png, jpg, gif, webp):
  the read returns a marker, and the image is attached to the next
  request as a user message (tool messages cannot carry images in
  any of the three APIs).

The model receives the image in its own format: chat gets an
`image_url` data URI, Responses gets `input_image`, Anthropic gets a
base64 image block. One image is capped at 10 MiB, ten per message.
Sessions store the image path, never the bytes; on `--resume` the
bytes are re-read from disk, so a deleted file leaves only the marker
text. A model without `"image"` in its `input` refuses the attach
with an error that says what to add. The GUI route is the RPC
`attach_image` request (see [rpc](rpc.md)).
