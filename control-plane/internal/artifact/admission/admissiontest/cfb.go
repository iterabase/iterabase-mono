package admissiontest

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"unicode/utf16"
)

// A minimal compound-file (MS-CFB v3) writer for generating .msg and
// encrypted-OOXML fixtures in code: 512-byte sectors, one FAT without DIFAT,
// a mini stream for streams under 4096 bytes, and a degenerate directory tree
// (every entry black, siblings chained through the right-sibling pointer).

const (
	cfbSectorSize    = 512
	cfbMiniSize      = 64
	cfbMiniCutoff    = 4096
	cfbMaxFATSectors = 109
	cfbEndOfChain    = 0xFFFFFFFE
	cfbFreeSector    = 0xFFFFFFFF
	cfbFATSector     = 0xFFFFFFFD
	cfbNoStream      = 0xFFFFFFFF
	cfbTypeStorage   = 1
	cfbTypeStream    = 2
	cfbTypeRoot      = 5
	cfbDirEntryBytes = 128
	cfbMaxNameUnits  = 31
	// cfbMaxIndex bounds every sector, entry, and mini index a fixture uses.
	cfbMaxIndex = 1 << 24
)

var cfbMagic = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}

// CFBNode is one storage or stream of a compound file fixture.
type CFBNode struct {
	name     string
	storage  bool
	data     []byte
	children []*CFBNode
}

// CFBStream returns a stream node holding data.
func CFBStream(name string, data []byte) *CFBNode { return &CFBNode{name: name, data: data} }

// CFBStorage returns a storage node holding children.
func CFBStorage(name string, children ...*CFBNode) *CFBNode {
	return &CFBNode{name: name, storage: true, children: children}
}

type cfbEntry struct {
	node               *CFBNode
	kind               byte
	left, right, child uint32
	start              uint32
	size               uint64
}

type cfbWriter struct {
	entries []*cfbEntry
	sectors [][]byte
	fat     []uint32
	mini    []byte
	miniFAT []uint32
}

// CFB serialises a compound file whose root storage holds children. Fixtures
// are small: it panics on a node name over 31 UTF-16 units or on a file too
// large for a DIFAT-free layout (about 7 MiB).
func CFB(children ...*CFBNode) []byte {
	w := &cfbWriter{}
	w.flatten(&CFBNode{name: "Root Entry", storage: true, children: children}, cfbTypeRoot)
	for _, e := range w.entries {
		if e.kind != cfbTypeStream {
			continue
		}
		e.size = uint64(len(e.node.data))
		switch {
		case len(e.node.data) == 0:
			e.start = cfbEndOfChain
		case len(e.node.data) < cfbMiniCutoff:
			e.start = w.allocMini(e.node.data)
		default:
			e.start = w.allocChain(e.node.data)
		}
	}
	root := w.entries[0]
	root.start = cfbEndOfChain
	if len(w.mini) > 0 {
		root.start, root.size = w.allocChain(w.mini), uint64(len(w.mini))
	}
	miniFATStart, miniFATSectors := uint32(cfbEndOfChain), 0
	if len(w.miniFAT) > 0 {
		raw := uint32s(w.miniFAT, cfbSectorSize/4)
		miniFATStart, miniFATSectors = w.allocChain(raw), len(raw)/cfbSectorSize
	}
	dirStart := w.allocChain(w.directory())
	fatSectors := 1
	for len(w.sectors)+fatSectors > fatSectors*cfbSectorSize/4 {
		fatSectors++
	}
	if fatSectors > cfbMaxFATSectors {
		panic(fmt.Sprintf("admissiontest: CFB fixture needs %d FAT sectors; at most %d are supported", fatSectors, cfbMaxFATSectors))
	}
	firstFAT := u32(len(w.sectors))
	for range fatSectors {
		w.fat = append(w.fat, cfbFATSector)
	}
	fatBytes := uint32s(w.fat, cfbSectorSize/4)
	var out bytes.Buffer
	out.Write(w.header(fatSectors, firstFAT, dirStart, miniFATStart, miniFATSectors))
	for _, sector := range w.sectors {
		out.Write(sector)
	}
	out.Write(fatBytes)
	return out.Bytes()
}

func (w *cfbWriter) flatten(n *CFBNode, kind byte) uint32 {
	if len(utf16.Encode([]rune(n.name))) > cfbMaxNameUnits {
		panic(fmt.Sprintf("admissiontest: CFB node name %q is longer than %d UTF-16 units", n.name, cfbMaxNameUnits))
	}
	id := u32(len(w.entries))
	entry := &cfbEntry{node: n, kind: kind, left: cfbNoStream, right: cfbNoStream, child: cfbNoStream}
	w.entries = append(w.entries, entry)
	var previous *cfbEntry
	for _, child := range n.children {
		childKind := byte(cfbTypeStream)
		if child.storage {
			childKind = cfbTypeStorage
		}
		childID := w.flatten(child, childKind)
		if previous == nil {
			entry.child = childID
		} else {
			previous.right = childID
		}
		previous = w.entries[childID]
	}
	return id
}

func (w *cfbWriter) allocChain(data []byte) uint32 {
	start := u32(len(w.sectors))
	for offset := 0; offset < len(data); offset += cfbSectorSize {
		sector := make([]byte, cfbSectorSize)
		copy(sector, data[offset:])
		w.sectors = append(w.sectors, sector)
		next := u32(len(w.sectors))
		if offset+cfbSectorSize >= len(data) {
			next = cfbEndOfChain
		}
		w.fat = append(w.fat, next)
	}
	return start
}

func (w *cfbWriter) allocMini(data []byte) uint32 {
	start := u32(len(w.mini) / cfbMiniSize)
	for offset := 0; offset < len(data); offset += cfbMiniSize {
		chunk := make([]byte, cfbMiniSize)
		copy(chunk, data[offset:])
		w.mini = append(w.mini, chunk...)
		next := u32(len(w.mini) / cfbMiniSize)
		if offset+cfbMiniSize >= len(data) {
			next = cfbEndOfChain
		}
		w.miniFAT = append(w.miniFAT, next)
	}
	return start
}

func (w *cfbWriter) directory() []byte {
	var out bytes.Buffer
	for _, e := range w.entries {
		raw := make([]byte, cfbDirEntryBytes)
		units := utf16.Encode([]rune(e.node.name))
		for i, unit := range units {
			binary.LittleEndian.PutUint16(raw[i*2:], unit)
		}
		// The name length is at most 64 bytes: flatten bounds the units.
		binary.LittleEndian.PutUint16(raw[64:], uint16(u32((len(units)+1)*2))) // #nosec G115 -- bounded by cfbMaxNameUnits.
		raw[66], raw[67] = e.kind, 1
		binary.LittleEndian.PutUint32(raw[68:], e.left)
		binary.LittleEndian.PutUint32(raw[72:], e.right)
		binary.LittleEndian.PutUint32(raw[76:], e.child)
		binary.LittleEndian.PutUint32(raw[116:], e.start)
		binary.LittleEndian.PutUint64(raw[120:], e.size)
		out.Write(raw)
	}
	for out.Len()%cfbSectorSize != 0 {
		raw := make([]byte, cfbDirEntryBytes)
		binary.LittleEndian.PutUint32(raw[68:], cfbNoStream)
		binary.LittleEndian.PutUint32(raw[72:], cfbNoStream)
		binary.LittleEndian.PutUint32(raw[76:], cfbNoStream)
		out.Write(raw)
	}
	return out.Bytes()
}

func (w *cfbWriter) header(fatSectors int, firstFAT, dirStart, miniFATStart uint32, miniFATSectors int) []byte {
	h := make([]byte, cfbSectorSize)
	copy(h, cfbMagic)
	binary.LittleEndian.PutUint16(h[24:], 0x003E)
	binary.LittleEndian.PutUint16(h[26:], 0x0003)
	binary.LittleEndian.PutUint16(h[28:], 0xFFFE)
	binary.LittleEndian.PutUint16(h[30:], 9)
	binary.LittleEndian.PutUint16(h[32:], 6)
	binary.LittleEndian.PutUint32(h[44:], u32(fatSectors))
	binary.LittleEndian.PutUint32(h[48:], dirStart)
	binary.LittleEndian.PutUint32(h[56:], cfbMiniCutoff)
	binary.LittleEndian.PutUint32(h[60:], miniFATStart)
	binary.LittleEndian.PutUint32(h[64:], u32(miniFATSectors))
	binary.LittleEndian.PutUint32(h[68:], cfbEndOfChain)
	for i := range cfbMaxFATSectors {
		value := uint32(cfbFreeSector)
		if i < fatSectors {
			value = firstFAT + u32(i)
		}
		binary.LittleEndian.PutUint32(h[76+i*4:], value)
	}
	return h
}

// uint32s encodes values little-endian, padded with free markers to a
// multiple of per entries.
func uint32s(values []uint32, per int) []byte {
	padded := append([]uint32{}, values...)
	for len(padded)%per != 0 {
		padded = append(padded, cfbFreeSector)
	}
	out := make([]byte, len(padded)*4)
	for i, v := range padded {
		binary.LittleEndian.PutUint32(out[i*4:], v)
	}
	return out
}

// u32 converts a fixture-bounded count or index; a value outside the
// fixture bound is a bug in the writer.
func u32(n int) uint32 {
	if n < 0 || n > cfbMaxIndex {
		panic(fmt.Sprintf("admissiontest: CFB index %d out of range", n))
	}
	return uint32(n) // #nosec G115 -- bounded above.
}
