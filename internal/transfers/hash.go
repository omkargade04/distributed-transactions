package transfers

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
)

// HashCanonical computes SHA-256 of a canonical JSON encoding of v.
//
// "Canonical" = object keys sorted, no whitespace. Two semantically-identical
// payloads with different formatting produce the same hash.
//
// Used by the idempotency layer to detect "same key, different payload" —
// which indicates a client bug and returns HTTP 422.
//
// TODO (you): implement.
//
// Approach:
//   1. Round-trip v through encoding/json:
//        - json.Marshal(v) → []byte
//        - json.Unmarshal back into a generic any
//      Why? To normalize Go-side variations (struct vs map). After unmarshal,
//      you get nested map[string]any with consistent shape.
//   2. Walk the generic value and write canonical JSON:
//        - For map[string]any: sort keys, write {"k1":v1,"k2":v2,...}
//        - For []any: write [item1,item2,...]
//        - For everything else: json.Marshal each primitive directly
//   3. SHA-256 the resulting []byte and return.
//
// See canonicalMarshal helper signature below — you implement it too.
//
// Pitfalls:
//   - json.Marshal a map[string]any already sorts keys (Go ≥ 1.12), BUT json.Marshal
//     a struct uses field order. Round-tripping through any gives you the map shape.
//   - For numbers, json.Unmarshal into any produces float64 — this is fine, as long
//     as canonicalMarshal uses json.Marshal which writes them the same way every time.
//   - For nested objects, you must recurse (not just one level).
func HashCanonical(v any) ([]byte, error) {
	// TODO: implement
	_ = sha256.Sum256
	_ = bytes.Buffer{}
	_ = sort.Strings
	return nil, fmt.Errorf("HashCanonical not implemented")
}

// canonicalMarshal serializes v with map keys sorted at every nesting level.
//
// TODO (you): implement.
//
// Hint — recursive type switch:
//
//   switch t := v.(type) {
//   case map[string]any:
//       // collect keys, sort, write {"k":v,...}
//   case []any:
//       // write [item,...]
//   default:
//       return json.Marshal(v)  // primitives: numbers, strings, bool, null
//   }
//
// Use bytes.Buffer to accumulate output, then return buf.Bytes().
func canonicalMarshal(v any) ([]byte, error) {
	// TODO: implement
	return json.Marshal(v) // placeholder — not actually canonical
}
