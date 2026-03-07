// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func walkMirror(b *testing.B) iter.Seq2[string, fs.FileInfo] {
	return func(yield func(string, fs.FileInfo) bool) {
		err := filepath.WalkDir("_mirror", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if strings.HasPrefix(d.Name(), ".") {
					return fs.SkipDir
				}
				return nil
			}
			// Only process JSON files
			if strings.HasPrefix(d.Name(), ".") || !strings.HasSuffix(d.Name(), ".json") {
				return nil
			}
			// Skip sync_state.json as it's internal metadata
			if d.Name() == "sync_state.json" {
				return nil
			}

			info, err := d.Info()
			if err != nil {
				return err
			}

			if !yield(path, info) {
				return fs.SkipAll
			}
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

func filterJSON(data []byte) []byte {
	var in map[string]any
	if err := json.Unmarshal(data, &in); err != nil {
		return data
	}
	out := make(map[string]any)

	// Basic fields
	for _, k := range []string{"id", "number", "title", "state", "body", "created_at", "updated_at", "closed_at", "event"} {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}

	// User/Actor login only
	for _, k := range []string{"user", "actor"} {
		if obj, ok := in[k].(map[string]any); ok {
			if login, ok := obj["login"]; ok {
				out[k] = map[string]any{"login": login}
			}
		}
	}

	// Label in events
	if label, ok := in["label"].(map[string]any); ok {
		if name, ok := label["name"]; ok {
			out["label"] = map[string]any{"name": name}
		}
	}

	// Labels in issues
	if labels, ok := in["labels"].([]any); ok {
		var outLabels []any
		for _, l := range labels {
			if lmap, ok := l.(map[string]any); ok {
				if name, ok := lmap["name"]; ok {
					outLabels = append(outLabels, map[string]any{"name": name})
				}
			}
		}
		if len(outLabels) > 0 {
			out["labels"] = outLabels
		}
	}

	b, err := json.Marshal(out)
	if err != nil {
		return data
	}
	return b
}

type testZip struct {
	path string
	time time.Duration
	size int64
}

func newTestZip(b *testing.B, method uint16, zipPath string, filter bool) testZip {
	start := time.Now()
	f, err := os.Create(zipPath)
	if err != nil {
		b.Fatal(err)
	}
	zw := zip.NewWriter(f)

	for path, info := range walkMirror(b) {
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			b.Fatal(err)
		}
		header.Name = filepath.ToSlash(path)
		header.Method = method

		writer, err := zw.CreateHeader(header)
		if err != nil {
			b.Fatal(err)
		}

		src, err := os.Open(path)
		if err != nil {
			b.Fatal(err)
		}
		data, err := io.ReadAll(src)
		src.Close()
		if err != nil {
			b.Fatal(err)
		}

		if filter {
			data = filterJSON(data)
		} else {
			var buf bytes.Buffer
			if err := json.Compact(&buf, data); err == nil {
				data = buf.Bytes()
			}
		}

		if _, err := writer.Write(data); err != nil {
			b.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}

	fi, err := os.Stat(zipPath)
	if err != nil {
		b.Fatal(err)
	}

	return testZip{
		path: zipPath,
		time: time.Since(start),
		size: fi.Size(),
	}
}

func (z testZip) Open(b *testing.B, mmap bool) (*zip.Reader, func()) {
	if mmap {
		f, err := os.Open(z.path)
		if err != nil {
			b.Fatal(err)
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			b.Fatal(err)
		}
		mmapData, err := syscall.Mmap(int(f.Fd()), 0, int(fi.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
		f.Close()
		if err != nil {
			b.Fatal(err)
		}
		zr, err := zip.NewReader(bytes.NewReader(mmapData), int64(len(mmapData)))
		if err != nil {
			syscall.Munmap(mmapData)
			b.Fatal(err)
		}
		return zr, func() { syscall.Munmap(mmapData) }
	}

	zrc, err := zip.OpenReader(z.path)
	if err != nil {
		b.Fatal(err)
	}
	return &zrc.Reader, func() { zrc.Close() }
}

type workerPool[T any] struct {
	concurrency int
	process     func(T)
	tasks       chan T
	wg          sync.WaitGroup
}

func newWorkerPool[T any](concurrency int, process func(T)) *workerPool[T] {
	p := &workerPool[T]{
		concurrency: concurrency,
		process:     process,
	}
	if p.concurrency > 1 {
		p.tasks = make(chan T, 1000)
		for w := 0; w < p.concurrency; w++ {
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				for t := range p.tasks {
					p.process(t)
				}
			}()
		}
	}
	return p
}

func (p *workerPool[T]) Submit(job T) {
	if p.concurrency > 1 {
		p.tasks <- job
	} else {
		p.process(job)
	}
}

func (p *workerPool[T]) Close() {
	if p.concurrency > 1 {
		close(p.tasks)
		p.wg.Wait()
	}
}

func runBenchIter(b *testing.B, z testZip, mmap bool, action string, concurrency string) {
	var zr *zip.Reader
	if z.path != "" {
		var cleanup func()
		zr, cleanup = z.Open(b, mmap)
		defer cleanup()
	}

	var count int64
	var totalBytes int64

	type task struct {
		path string
		zf   *zip.File
	}

	process := func(t task) {
		var rc io.ReadCloser
		var err error
		if t.zf != nil {
			rc, err = t.zf.Open()
		} else {
			rc, err = os.Open(t.path)
		}
		if err != nil {
			b.Fatal(err)
		}

		if action == "decode" {
			type Entry struct {
				ID int64 `json:"id"`
			}
			var e Entry
			_ = json.NewDecoder(rc).Decode(&e)
		} else {
			_, _ = io.ReadAll(rc)
		}
		rc.Close()
	}

	concurrencyLevel := 1
	if concurrency == "par" {
		concurrencyLevel = runtime.GOMAXPROCS(0)
	}

	pool := newWorkerPool(concurrencyLevel, process)

	if zr == nil {
		for path, info := range walkMirror(b) {
			count++
			totalBytes += info.Size()
			pool.Submit(task{path: path})
		}
	} else {
		for _, zf := range zr.File {
			count++
			totalBytes += int64(zf.UncompressedSize64)
			pool.Submit(task{zf: zf})
		}
	}

	pool.Close()

	b.ReportMetric(float64(count), "files")
	b.SetBytes(totalBytes)
}

// Benchmark several ways of reading the issue mirror.
func BenchmarkMirror(b *testing.B) {
	parentTempDir := b.TempDir()

	type zipKey struct {
		method uint16
		filter bool
	}
	zips := make(map[zipKey]testZip)

	getZip := func(innerB *testing.B, method uint16, filter bool) testZip {
		k := zipKey{method, filter}
		if z, ok := zips[k]; ok {
			return z
		}
		name := "store"
		if method == zip.Deflate {
			name = "deflate"
		}
		if filter {
			name = "filtered_" + name
		}
		z := newTestZip(innerB, method, filepath.Join(parentTempDir, name+".zip"), filter)
		zips[k] = z
		return z
	}

	for _, src := range []string{"FS", "zip"} {
		methods := []uint16{0}
		if src == "zip" {
			methods = []uint16{zip.Store, zip.Deflate}
		}
		for _, method := range methods {
			for _, filtered := range []bool{false, true} {
				if src == "FS" && filtered {
					continue // filtering only applies to zip in this benchmark
				}

				for _, mmap := range []bool{false, true} {
					if src == "FS" && mmap {
						continue // mmap only applies to zip in this benchmark
					}

					for _, action := range []string{"read", "decode"} {
						for _, concurrency := range []string{"seq", "par"} {
							name := "src=" + src
							if src == "zip" {
								switch method {
								case zip.Store:
									name += "/method=store"
								case zip.Deflate:
									name += "/method=deflate"
								}
								name += fmt.Sprintf("/filtered=%v/mmap=%v", filtered, mmap)
							}
							name += "/action=" + action + "/order=" + concurrency

							b.Run(name, func(b *testing.B) {
								var z testZip
								if src == "zip" {
									z = getZip(b, method, filtered)
								}

								for b.Loop() {
									runBenchIter(b, z, mmap, action, concurrency)
								}

								if z.path != "" {
									b.ReportMetric(float64(z.time.Seconds()), "setup-s")
									b.ReportMetric(float64(z.size)/(1024*1024), "zip-MB")
								}
							})
						}
					}
				}
			}
		}
	}
}
