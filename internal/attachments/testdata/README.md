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

## BUG-2963 fixtures — formats the sniffer learned to recognise

Same rule as above: real encoder output, truncated to the 512 bytes
`SniffMIME` actually reads. `bzip2.head512` and `sevenzip.head512` are shorter
than 512 because the whole file is.

| file | produced by | what it proves |
|---|---|---|
| `tar.head512` | GNU `tar -cf` (tar 1.34, Linux) | `ustar` at offset 257 — the magic is not at the start, which is why no prefix matcher finds it |
| `bzip2.head512` | `bzip2 -c` (1.0.8), complete file (88 B) | `BZh` plus the block-size digit |
| `sevenzip.head512` | `py7zr` 1.1.3, complete file (192 B) | the six-byte 7z signature |
| `flac.head512` | libsndfile 1.2.2 via `soundfile`, FLAC format | the `fLaC` stream marker |
| `aac-adts.head512` | FFmpeg 7.1 `-c:a aac -f adts` | an ADTS sync (`ff f1`) — the one signature weak enough to need its extension |
| `matroska.head512` | FFmpeg 7.1 `-f matroska` | EBML magic with DocType `matroska` at offset 24 |
| `webm.head512` | FFmpeg 7.1 `-f webm` | EBML magic with DocType `webm` at offset 24 — the control that stops the DocType read from answering Matroska for everything |
| `avi.head512` | FFmpeg 7.1 `-f avi` | RIFF/AVI, which the stdlib names `video/avi` against the allowlist's `video/x-msvideo` |
| `ogg-opus.head512` | FFmpeg 7.1 `-c:a libopus -f ogg` | `OggS`, which the stdlib names `application/ogg` against the allowlist's `audio/ogg` |

The FFmpeg used is the one bundled with Remotion
(`@remotion/compositor-linux-x64-gnu`), transcoded from real project assets;
there is no system FFmpeg on the machine these were made on.

**Gap, recorded rather than papered over:** every media fixture here comes from
one FFmpeg build. A second encoder would be worth having for the EBML pair in
particular, since the DocType read is the only check here that depends on where
a muxer places a string rather than on a fixed prefix.
