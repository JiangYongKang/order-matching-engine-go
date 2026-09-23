package engine

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
)

// Journal is an append-only file of framed, gob-encoded Events.
//
// Frame layout (little-endian):
//
//	uint32 magic  (0x4D454431 "MED1")
//	uint32 length (payload bytes)
//	uint32 crc32  (IEEE checksum of payload)
//	bytes  payload (one gob-encoded Event)
//
// A torn trailing frame (process killed mid-write) fails the length/CRC
// validation and is truncated on open, so recovery resumes exactly at the
// last fully durable event.
type Journal struct {
	path string
	f    *os.File
}

const (
	journalMagic = 0x4D454431 // "MED1"
	frameHeader  = 12         // uint32 magic + uint32 length + uint32 crc32
)

// OpenJournal opens (creating if needed) the event file, drops any torn
// trailing frame, and returns the durable events.
func OpenJournal(path string) (*Journal, []*Event, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open journal: %w", err)
	}
	j := &Journal{path: path, f: f}

	events, validBytes, err := readFramesFrom(f)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if fi, statErr := f.Stat(); statErr == nil && fi.Size() != validBytes {
		// Persist the truncation so later readers do not see the torn tail.
		if terr := f.Truncate(validBytes); terr != nil {
			f.Close()
			return nil, nil, fmt.Errorf("truncate torn journal: %w", terr)
		}
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("seek journal: %w", err)
	}
	return j, events, nil
}

// ReadJournal reads events from a journal file without modifying it.
func ReadJournal(path string) ([]*Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read journal: %w", err)
	}
	defer f.Close()
	events, _, err := readFramesFrom(f)
	return events, err
}

// readFramesFrom returns decoded events and the byte offset up to which all
// frames were valid. A short/CRC-failed tail is treated as a torn write and
// stops reading at that boundary (validBytes points before it).
func readFramesFrom(r io.ReaderAt) ([]*Event, int64, error) {
	data, err := io.ReadAll(io.NewSectionReader(r, 0, 1<<63-1))
	if err != nil {
		return nil, 0, fmt.Errorf("read journal: %w", err)
	}
	var events []*Event
	var off int64
	for int(off)+frameHeader <= len(data) {
		magic := binary.LittleEndian.Uint32(data[off : off+4])
		if magic != journalMagic {
			// Unknown/torn bytes: stop and discard the tail.
			return events, off, nil
		}
		length := int(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		if length < 0 || int(off)+frameHeader+length > len(data) {
			// Partial frame (torn write): keep only what is complete.
			return events, off, nil
		}
		crc := binary.LittleEndian.Uint32(data[off+8 : off+12])
		payload := data[off+frameHeader : off+frameHeader+int64(length)]
		if crc32.ChecksumIEEE(payload) != crc {
			// Checksum failure: treat tail as unusable.
			return events, off, nil
		}
		ev, derr := decodeEvent(payload)
		if derr != nil {
			return nil, off, fmt.Errorf("decode event at offset %d: %w", off, derr)
		}
		events = append(events, ev)
		off += int64(frameHeader + length)
	}
	return events, off, nil
}

// Append durably writes one framed event (data + fsync before return).
func (j *Journal) Append(ev *Event) error {
	if j.f == nil {
		return errJournalClosed
	}
	payload, err := encodeEvent(ev)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	hdr := make([]byte, frameHeader)
	binary.LittleEndian.PutUint32(hdr[0:4], journalMagic)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(payload)))
	binary.LittleEndian.PutUint32(hdr[8:12], crc32.ChecksumIEEE(payload))
	if _, err := j.f.Write(append(hdr, payload...)); err != nil {
		return fmt.Errorf("write event: %w", err)
	}
	if err := j.f.Sync(); err != nil {
		return fmt.Errorf("sync journal: %w", err)
	}
	return nil
}

// Close flushes and closes the file. Safe to call once.
func (j *Journal) Close() error {
	if j == nil || j.f == nil {
		return nil
	}
	err := j.f.Close()
	j.f = nil
	return err
}

func encodeEvent(ev *Event) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(ev); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeFrame(blob []byte) (*Event, error) {
	return decodeEvent(blob)
}

func decodeEvent(blob []byte) (*Event, error) {
	var ev Event
	if err := gob.NewDecoder(bytes.NewReader(blob)).Decode(&ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

// errJournalClosed is returned when appending after close.
var errJournalClosed = errors.New("journal is closed")

func crc32IEEE(p []byte) uint32 { return crc32.ChecksumIEEE(p) }
