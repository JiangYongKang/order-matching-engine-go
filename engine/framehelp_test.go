package engine

import "encoding/binary"

func binaryPutMagic(hdr []byte)     { binary.LittleEndian.PutUint32(hdr[0:4], journalMagic) }
func putLE32(dst []byte, v uint32)  { binary.LittleEndian.PutUint32(dst, v) }
func crc32Checksum(p []byte) uint32 { return crc32IEEE(p) }
