package vdp

import (
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// ErrIntegrityMismatch is returned when template bytes do not match their
// integrity metadata (§3.6). The mismatch is treated as a template fetch
// failure for the affected slot (§9.1).
var ErrIntegrityMismatch = errors.New("template integrity verification failed")

// sriAlgorithms ranks the hash algorithms W3C Subresource Integrity defines,
// weakest first. Unknown algorithms are ignored entirely.
var sriAlgorithms = map[string]int{"sha256": 1, "sha384": 2, "sha512": 3}

// verifyIntegrity checks body against SRI metadata (§3.6), following W3C SRI
// semantics: of the parseable tokens, only the strongest algorithm counts, and
// any one matching digest of that algorithm passes. Metadata containing no
// parseable token imposes no constraint, matching browser SRI behavior.
func verifyIntegrity(metadata string, body []byte) error {
	strongest := 0
	var algo string
	var digests []string
	for _, token := range strings.Fields(metadata) {
		a, rest, ok := strings.Cut(token, "-")
		if !ok {
			continue
		}
		rank, known := sriAlgorithms[a]
		if !known {
			continue
		}
		digest, _, _ := strings.Cut(rest, "?") // Options are ignored.
		if rank > strongest {
			strongest, algo, digests = rank, a, nil
		}
		if rank == strongest {
			digests = append(digests, digest)
		}
	}
	if strongest == 0 {
		return nil
	}

	var sum []byte
	switch algo {
	case "sha256":
		s := sha256.Sum256(body)
		sum = s[:]
	case "sha384":
		s := sha512.Sum384(body)
		sum = s[:]
	case "sha512":
		s := sha512.Sum512(body)
		sum = s[:]
	}
	want := base64.StdEncoding.EncodeToString(sum)
	for _, d := range digests {
		if subtle.ConstantTimeCompare([]byte(d), []byte(want)) == 1 {
			return nil
		}
	}
	return fmt.Errorf("%w: no %s digest matches", ErrIntegrityMismatch, algo)
}
