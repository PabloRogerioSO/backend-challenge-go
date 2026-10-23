package hashing

import "testing"

func TestHashCanonical(t *testing.T) {
	type typed struct {
		B int            `json:"b"`
		A string         `json:"a"`
		N map[string]int `json:"n"`
	}

	fromStruct, err := HashCanonical(typed{B: 2, A: "x<y&z", N: map[string]int{"z": 1, "a": 2}})
	if err != nil {
		t.Fatalf("HashCanonical struct: %v", err)
	}
	fromMap, err := HashCanonical(map[string]any{
		"a": "x<y&z",
		"n": map[string]any{"a": 2, "z": 1},
		"b": 2,
	})
	if err != nil {
		t.Fatalf("HashCanonical map: %v", err)
	}
	if fromStruct != fromMap {
		t.Fatalf("struct and map with the same content must hash equally: %s vs %s", fromStruct, fromMap)
	}
	if len(fromStruct) != 64 {
		t.Fatalf("hash length = %d, want 64", len(fromStruct))
	}

	changed, err := HashCanonical(typed{B: 3, A: "x<y&z", N: map[string]int{"z": 1, "a": 2}})
	if err != nil {
		t.Fatalf("HashCanonical changed: %v", err)
	}
	if changed == fromStruct {
		t.Fatal("different payloads must hash differently")
	}

	again, err := HashCanonical(typed{B: 2, A: "x<y&z", N: map[string]int{"a": 2, "z": 1}})
	if err != nil || again != fromStruct {
		t.Fatalf("hash must be stable: %s (%v)", again, err)
	}

	if _, err := HashCanonical(make(chan int)); err == nil {
		t.Fatal("unmarshalable payload must return an error")
	}
}
