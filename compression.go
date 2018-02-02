// Copyright 2017 The Gorilla WebSocket Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package websocket

import (
	"errors"
	"io"
	"strings"
	"sync"

	"compress/flate"
)

const (
	minCompressionLevel     = -2 // flate.HuffmanOnly not defined in Go < 1.6
	maxCompressionLevel     = flate.BestCompression
	defaultCompressionLevel = 1

	tail =
	// Add four bytes as specified in RFC
	"\x00\x00\xff\xff" +
		// Add final block to squelch unexpected EOF error from flate reader.
		"\x01\x00\x00\xff\xff"
)

var (
	flateWriterPools [maxCompressionLevel - minCompressionLevel + 1]sync.Pool
	flateReaderPool  = sync.Pool{New: func() interface{} {
		return flate.NewReader(nil)
	}}
)

func decompressNoContextTakeover(r io.Reader) io.ReadCloser {
	fr, _ := flateReaderPool.Get().(io.ReadCloser)
	fr.(flate.Resetter).Reset(io.MultiReader(r, strings.NewReader(tail)), nil)
	return &flateReadWrapper{fr: fr}
}

func isValidCompressionLevel(level int) bool {
	return minCompressionLevel <= level && level <= maxCompressionLevel
}

func compressNoContextTakeover(w io.WriteCloser, level int) io.WriteCloser {
	p := &flateWriterPools[level-minCompressionLevel]
	tw := &truncWriter{w: w}
	fw, _ := p.Get().(*flate.Writer)
	if fw == nil {
		fw, _ = flate.NewWriter(tw, level)
	} else {
		fw.Reset(tw)
	}
	return &flateWriteWrapper{fw: fw, tw: tw, p: p}
}

// truncWriter is an io.Writer that writes all but the last four bytes of the
// stream to another io.Writer.
type truncWriter struct {
	w io.WriteCloser
	n int
	p [4]byte
}

func (w *truncWriter) Write(p []byte) (int, error) {
	n := 0

	// fill buffer first for simplicity.
	if w.n < len(w.p) {
		n = copy(w.p[w.n:], p)
		p = p[n:]
		w.n += n
		if len(p) == 0 {
			return n, nil
		}
	}

	m := len(p)
	if m > len(w.p) {
		m = len(w.p)
	}

	if nn, err := w.w.Write(w.p[:m]); err != nil {
		return n + nn, err
	}

	copy(w.p[:], w.p[m:])
	copy(w.p[len(w.p)-m:], p[len(p)-m:])
	nn, err := w.w.Write(p[:len(p)-m])
	return n + nn, err
}

type flateWriteWrapper struct {
	fw *flate.Writer
	tw *truncWriter
	p  *sync.Pool
}

func (w *flateWriteWrapper) Write(p []byte) (int, error) {
	if w.fw == nil {
		return 0, errWriteClosed
	}

	return w.fw.Write(p)
}

func (w *flateWriteWrapper) Close() error {
	if w.fw == nil {
		return errWriteClosed
	}
	err1 := w.fw.Flush()

	w.p.Put(w.fw)
	w.fw = nil

	if w.tw.p != [4]byte{0, 0, 0xff, 0xff} {
		return errors.New("websocket: internal error, unexpected bytes at end of flate stream")
	}

	w.tw.p = [4]byte{}
	w.tw.n = 0

	err2 := w.tw.w.Close()
	if err1 != nil {
		return err1
	}

	return err2
}

type flateReadWrapper struct {
	fr io.ReadCloser
}

func (r *flateReadWrapper) Read(p []byte) (int, error) {
	if r.fr == nil {
		return 0, io.ErrClosedPipe
	}

	n, err := r.fr.Read(p)

	if err == io.EOF {
		// Preemptively place the reader back in the pool. This helps with
		// scenarios where the application does not call NextReader() soon after
		// this final read.
		r.Close()
	}

	return n, err
}

func (r *flateReadWrapper) Close() error {
	if r.fr == nil {
		return io.ErrClosedPipe
	}
	err := r.fr.Close()

	flateReaderPool.Put(r.fr)

	r.fr = nil
	return err
}

type (
	contextTakeoverWriterFactory struct {
		fw *flate.Writer
		tw truncWriter
	}

	flateTakeoverWriteWrapper struct {
		f *contextTakeoverWriterFactory
	}
)

func (f *contextTakeoverWriterFactory) newCompressionWriter(w io.WriteCloser, level int) io.WriteCloser {
	f.tw.w = w
	f.tw.n = 0
	return &flateTakeoverWriteWrapper{f}
}

func (w *flateTakeoverWriteWrapper) Write(p []byte) (int, error) {
	if w.f == nil {
		return 0, errWriteClosed
	}
	return w.f.fw.Write(p)
}

func (w *flateTakeoverWriteWrapper) Close() error {
	if w.f == nil {
		return errWriteClosed
	}
	f := w.f
	w.f = nil
	err1 := f.fw.Flush()
	if f.tw.p != [4]byte{0, 0, 0xff, 0xff} {
		return errors.New("websocket: internal error, unexpected bytes at end of flate stream")
	}
	err2 := f.tw.w.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

type (
	contextTakeoverReaderFactory struct {
		fr     io.ReadCloser
		window []byte
	}

	flateTakeoverReadWrapper struct {
		f *contextTakeoverReaderFactory
	}
)

func (f *contextTakeoverReaderFactory) newDeCompressionReader(r io.Reader) io.ReadCloser {
	f.fr.(flate.Resetter).Reset(io.MultiReader(r, strings.NewReader(tail)), f.window)
	return &flateTakeoverReadWrapper{f}
}

func (r *flateTakeoverReadWrapper) Read(p []byte) (int, error) {
	if r.f.fr == nil {
		return 0, io.ErrClosedPipe
	}

	n, err := r.f.fr.Read(p)

	// add window
	r.f.window = append(r.f.window, p[:n]...)
	if len(r.f.window) > maxWindowBits {
		offset := len(r.f.window) - maxWindowBits
		r.f.window = r.f.window[offset:]
	}

	if err == io.EOF {
		// Preemptively place the reader back in the pool. This helps with
		// scenarios where the application does not call NextReader() soon after
		// this final read.
		r.Close()
	}

	return n, err
}

func (r *flateTakeoverReadWrapper) Close() error {
	if r.f.fr == nil {
		return io.ErrClosedPipe
	}
	err := r.f.fr.Close()
	return err
}
