package matching

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"strconv"
	"strings"
)

// 流水持久化错误，便于调用方区分损坏类型。
var (
	ErrJournalFormat    = errors.New("journal: malformed record line")
	ErrJournalChecksum  = errors.New("journal: checksum mismatch")
	ErrJournalTornWrite = fmt.Errorf("%w: unterminated trailing record (crash during append)", io.ErrUnexpectedEOF)
)

// Journal 基于 JSON Lines 的事件流水，每行形如 <crc32-hex>|<json>。
// 每条记录 append 后立即 flush + fsync：只有完整落盘并带换行的记录才会被
// 回放接受，崩溃导致的半截行在恢复时被截断，不产生任何状态副作用。
type Journal struct {
	path string
	f    *os.File
	w    *bufio.Writer
}

func openJournal(path string) (*Journal, error) {
	if path == "" {
		return nil, errors.New("journal: empty path")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Journal{path: path, f: f, w: bufio.NewWriterSize(f, 64*1024)}, nil
}

func (j *Journal) append(r *JournalRecord) error {
	line, err := encodeRecord(r)
	if err != nil {
		return err
	}
	if _, err := j.w.WriteString(line); err != nil {
		return err
	}
	if err := j.w.Flush(); err != nil {
		return err
	}
	return j.f.Sync()
}

func (j *Journal) Close() error {
	if err := j.w.Flush(); err != nil {
		_ = j.f.Close()
		return err
	}
	return j.f.Close()
}

func encodeRecord(r *JournalRecord) (string, error) {
	payload, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	sum := crc32.ChecksumIEEE(payload)
	return fmt.Sprintf("%08x|%s\n", sum, payload), nil
}

func decodeRecord(line string) (*JournalRecord, error) {
	idx := strings.IndexByte(line, '|')
	if idx <= 0 {
		return nil, fmt.Errorf("%w: missing checksum separator", ErrJournalFormat)
	}
	want, err := strconv.ParseUint(line[:idx], 16, 32)
	if err != nil {
		return nil, fmt.Errorf("%w: bad checksum: %v", ErrJournalFormat, err)
	}
	payload := []byte(line[idx+1:])
	if crc32.ChecksumIEEE(payload) != uint32(want) {
		return nil, ErrJournalChecksum
	}
	var r JournalRecord
	if err := json.Unmarshal(payload, &r); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrJournalFormat, err)
	}
	if len(r.Events) == 0 {
		return nil, fmt.Errorf("%w: empty events", ErrJournalFormat)
	}
	return &r, nil
}

// scanRecords 顺序读取全部完整记录并回调 fn。
// 文件末尾存在未以换行结束的行时返回 ErrJournalTornWrite（该行绝不施加）。
func scanRecords(path string, fn func(*JournalRecord) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 1<<20)
	off := int64(0)
	lineNo := 0
	for {
		line, readErr := r.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		if len(line) > 0 {
			lineNo++
			if readErr == io.EOF {
				// 最后一行没有换行符：属于崩溃时的半截写入，拒绝施加。
				if len(bytes.TrimSpace(line)) > 0 {
					return fmt.Errorf("%w (line=%d offset=%d)", ErrJournalTornWrite, lineNo, off)
				}
				return nil
			}
			rec, err := decodeRecord(strings.TrimSpace(string(line)))
			if err != nil {
				return fmt.Errorf("journal %s line %d offset %d: %w", path, lineNo, off, err)
			}
			if err := fn(rec); err != nil {
				return fmt.Errorf("journal %s line %d: %w", path, lineNo, err)
			}
			off += int64(len(line))
		}
		if readErr == io.EOF {
			break
		}
	}
	return nil
}
