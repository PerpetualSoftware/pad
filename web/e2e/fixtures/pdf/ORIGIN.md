# PDF fixtures: origin and licence

Two real-world PDFs from the pdf.js project's own test corpus, used by
`web/e2e/attachment-pdf-preview.spec.ts` (E3) to prove that JPEG2000 (JPX) and
JBIG2 images decode through the no-wasm fallbacks under Pad's CSP.

| file | what it exercises | bytes | sha256 |
|---|---|---|---|
| `bug_jpx.pdf` | a JPXDecode image | 5451 | 4010d808557a72278368c05518367735ed186cbdd04974b8506275f9ac40f999 |
| `jbig2_symbol_offset.pdf` | a JBIG2Decode image | 1157 | 305b71c67829ef0bb8bddab9fa3ee763dcecd173f5fd996740d17212888fa97d |

- Source: https://github.com/mozilla/pdf.js, `test/pdfs/`, downloaded from
  `https://raw.githubusercontent.com/mozilla/pdf.js/f99d13113d28d692e33de727d622582cd6e83cf7/test/pdfs/<file>`
- Commit: `f99d13113d28d692e33de727d622582cd6e83cf7` (master at download, 2026-10-26)
- Licence: Apache License 2.0, the licence of the pdf.js repository these files
  are distributed in. The licence text is `LICENSE-pdfjs` beside this note,
  copied unmodified from the same commit. The files are unmodified.

The other fixtures the spec uses (the multi-page, hostile-action and oversize
PDFs) are generated as text by `web/e2e/lib/pdf-fixtures.ts`.
