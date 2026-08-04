# Vendored VDP test corpus

Copied verbatim from the VDP repository's `tests/` directory at tag
`v0.2.0-alpha`. These fixtures are canonical there — do not edit them here.
When the VDP corpus changes, re-copy:

    cp -r ../VDP/tests/{transforms,rendering,descriptors} vdp/testdata/corpus/

`vdp/corpus_test.go` runs every case; agreement with the VDP reference runner
(`tests/run-transforms.mjs`) is what proves cross-implementation identity.
