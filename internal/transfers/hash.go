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
// Invariant examples:
//
//	A := map[string]any{"amount_minor": 1500, "payer_id": "acc_001", "payee_id": "acc_002"}
//	B := map[string]any{"payer_id": "acc_001", "amount_minor": 1500, "payee_id": "acc_002"}
//	C := map[string]any{"payer_id": "acc_001", "amount_minor": 9999, "payee_id": "acc_002"}
//
//	hash(A) == hash(B)  // same fields, different key order → same canonical → same hash → REPLAY served
//	hash(A) != hash(C)  // different amount → different canonical → different hash → 422 returned
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
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal for normalization: %w", err)
	}

	var normalized any
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return nil, fmt.Errorf("unmarshal for normalization: %w", err)
	}

	canonicalBytes, err := canonicalMarshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("canonical marshal: %w", err)
	}

	hash := sha256.Sum256(canonicalBytes)
	return hash[:], nil
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
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		var buf bytes.Buffer
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			keyBytes, err := json.Marshal(k)
			if err != nil {
				return nil, err
			}
			buf.Write(keyBytes)
			buf.WriteByte(':')

			valBytes, err := canonicalMarshal(t[k])
			if err != nil {
				return nil, err
			}
			buf.Write(valBytes)
		}
		buf.WriteByte('}')
		return buf.Bytes(), nil

	case []any:
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			itemBytes, err := canonicalMarshal(item)
			if err != nil {
				return nil, err
			}
			buf.Write(itemBytes)
		}
		buf.WriteByte(']')
		return buf.Bytes(), nil

	default:
		return json.Marshal(v)
	}
}
