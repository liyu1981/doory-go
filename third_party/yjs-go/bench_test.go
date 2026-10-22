package yjs_test

import (
	"fmt"
	"testing"

	yjs "github.com/CivNode/yjs-go"
)

// BenchmarkTextInsert measures sequential insert performance.
func BenchmarkTextInsert1k(b *testing.B) {
	benchTextInsert(b, 1_000)
}

func BenchmarkTextInsert10k(b *testing.B) {
	benchTextInsert(b, 10_000)
}

func BenchmarkTextInsert100k(b *testing.B) {
	benchTextInsert(b, 100_000)
}

func benchTextInsert(b *testing.B, n int) {
	b.Helper()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		doc := yjs.NewDoc()
		text := doc.GetText("code")
		doc.Transact(func() {
			for j := 0; j < n; j++ {
				text.Insert(uint64(j), "a")
			}
		}, nil)
	}
}

// BenchmarkTextAppend measures appending to the end (common case).
func BenchmarkTextAppend1k(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		doc := yjs.NewDoc()
		text := doc.GetText("code")
		for j := 0; j < 1_000; j++ {
			doc.Transact(func() {
				text.Insert(text.Len(), "a")
			}, nil)
		}
	}
}

// BenchmarkMapUpdate measures setting keys in a YMap.
func BenchmarkMapUpdate1k(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		doc := yjs.NewDoc()
		m := doc.GetMap("data")
		for j := 0; j < 1_000; j++ {
			doc.Transact(func() {
				m.Set(fmt.Sprintf("key%d", j), j)
			}, nil)
		}
	}
}

// BenchmarkSnapshotSize measures the encoded update size after N inserts.
func BenchmarkSnapshotSize(b *testing.B) {
	sizes := []int{100, 1_000, 10_000}
	for _, n := range sizes {
		b.Run(fmt.Sprintf("%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				doc := yjs.NewDoc()
				text := doc.GetText("code")
				var lastUpdate []byte
				doc.OnUpdate(func(u []byte, _ interface{}) { lastUpdate = u })
				doc.Transact(func() {
					for j := 0; j < n; j++ {
						text.Insert(uint64(j), "a")
					}
				}, nil)
				b.SetBytes(int64(len(lastUpdate)))
			}
		})
	}
}

// BenchmarkApplyUpdate measures how fast remote updates can be integrated.
func BenchmarkApplyUpdate(b *testing.B) {
	// Pre-generate an update with 1000 inserts.
	src := yjs.NewDoc()
	text := src.GetText("code")
	var update []byte
	src.OnUpdate(func(u []byte, _ interface{}) { update = u })
	src.Transact(func() {
		for j := 0; j < 1_000; j++ {
			text.Insert(uint64(j), "a")
		}
	}, nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst := yjs.NewDoc()
		if err := yjs.ApplyUpdate(dst, update, "bench"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEncodeStateVector measures state vector encoding.
func BenchmarkEncodeStateVector(b *testing.B) {
	doc := yjs.NewDoc()
	text := doc.GetText("code")
	doc.Transact(func() {
		for j := 0; j < 1_000; j++ {
			text.Insert(uint64(j), "a")
		}
	}, nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := yjs.EncodeStateVector(doc)
		if err != nil {
			b.Fatal(err)
		}
	}
}
