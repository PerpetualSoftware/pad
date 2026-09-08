# Attachment fixtures

Real files, not hand-built headers. A sniffer's whole job is to recognise what
encoders actually emit, so a fixture I typed from a spec would test my reading
of the spec (BUG-2961's tests, `mime_isobmff_test.go`).

| file | produced by | ftyp brands |
|---|---|---|
| `apple-still-heic.head512` | `sips -s format heic` on macOS 26 — **first 512 bytes only** | major `heic`; compatible `mif1 MiPr miaf MiHB heic` |
| `apple-still.avif` | `sips -s format avif` on macOS 26, complete file (509 B) | major `avif`; compatible `MiPr avif miaf mif1` |
| `still.avif` | `heif-enc -A -q 60` (libheif 1.20.2 / aom 3.12.1) on Linux, complete file | major `avif`; compatible `mif1 avif miaf` |
| `still-mif1.heif` | `heif-enc -U` (libheif 1.20.2, uncompressed codec) on Linux, complete file | major `mif1`; compatible `mif1 miaf` |

The `.head512` file is a prefix because that is the entire input domain of
`SniffMIME` — it reads at most 512 bytes and the `ftyp` box is the first box in
the file. It is named for what it is so nobody mistakes it for a loadable image.

Two gaps, recorded rather than papered over:

- **No `sips`-produced HEIC beyond 512 bytes and no iPhone-camera capture.**
  Both Apple fixtures come from `sips`, not from a camera: same container and
  brand set, different encoder pipeline.
- **No file whose MAJOR brand is `mif1` from Apple.** `sips -s format heif`
  writes no file on that machine. The libheif `still-mif1.heif` covers the
  major-`mif1` shape from a different encoder instead of a guessed one.
