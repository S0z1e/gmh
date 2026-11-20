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
Sessions store a SHA-256 digest, media type and origin path. Image bytes
are saved separately in private, immutable blobs under
`~/.minimal-harness/blobs/sha256/`, shared across sessions by digest.
Resuming or switching sessions loads those saved bytes; changing or deleting
the origin file does not change history. Missing or corrupt blobs, and legacy
path-only attachments, are explicitly marked unavailable. Their origin paths
are never re-read as a fallback. Back up the blob directory with the sessions.
A model without `"image"` in its `input` refuses the attach
with an error that says what to add. The GUI route is the RPC
`attach_image` request (see [rpc](rpc.md)).
