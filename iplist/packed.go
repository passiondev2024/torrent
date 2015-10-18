package iplist

import (
	"encoding/binary"
	"io"
	"net"
)

// The packed format is an 8 byte integer of the number of ranges. Then 20
// bytes per range, consisting of 4 byte packed IP being the lower bound IP of
// the range, then 4 bytes of the upper, inclusive bound, 8 bytes for the
// offset of the description from the end of the packed ranges, and 4 bytes
// for the length of the description. After these packed ranges, are the
// concatenated descriptions.

const (
	packedRangesOffset = 8
	packedRangeLen     = 20
)

func (me *IPList) WritePacked(w io.Writer) (err error) {
	descOffsets := make(map[string]int64, len(me.ranges))
	descs := make([]string, 0, len(me.ranges))
	var nextOffset int64
	// This is a little monadic, no?
	write := func(b []byte, expectedLen int) {
		if err != nil {
			return
		}
		var n int
		n, err = w.Write(b)
		if err != nil {
			return
		}
		if n != expectedLen {
			panic(n)
		}
	}
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(len(me.ranges)))
	write(b[:], 8)
	for _, r := range me.ranges {
		write(r.First.To4(), 4)
		write(r.Last.To4(), 4)
		descOff, ok := descOffsets[r.Description]
		if !ok {
			descOff = nextOffset
			descOffsets[r.Description] = descOff
			descs = append(descs, r.Description)
			nextOffset += int64(len(r.Description))
		}
		binary.LittleEndian.PutUint64(b[:], uint64(descOff))
		write(b[:], 8)
		binary.LittleEndian.PutUint32(b[:], uint32(len(r.Description)))
		write(b[:4], 4)
	}
	for _, d := range descs {
		write([]byte(d), len(d))
	}
	return
}

func NewFromPacked(b []byte) PackedIPList {
	return PackedIPList(b)
}

type PackedIPList []byte

var _ Ranger = PackedIPList{}

func (me PackedIPList) len() int {
	return int(binary.LittleEndian.Uint64(me[:8]))
}

func (me PackedIPList) NumRanges() int {
	return me.len()
}

func (me PackedIPList) getFirst(i int) net.IP {
	off := packedRangesOffset + packedRangeLen*i
	return net.IP(me[off : off+4])
}

func (me PackedIPList) getRange(i int) (ret Range) {
	rOff := packedRangesOffset + packedRangeLen*i
	last := me[rOff+4 : rOff+8]
	descOff := int(binary.LittleEndian.Uint64(me[rOff+8:]))
	descLen := int(binary.LittleEndian.Uint32(me[rOff+16:]))
	descOff += packedRangesOffset + packedRangeLen*me.len()
	ret = Range{
		me.getFirst(i),
		net.IP(last),
		string(me[descOff : descOff+descLen]),
	}
	return
}

func (me PackedIPList) Lookup(ip net.IP) (r Range, ok bool) {
	ip4 := ip.To4()
	if ip4 == nil {
		// If the IP list was built successfully, then it only contained IPv4
		// ranges. Therefore no IPv6 ranges are blocked.
		if ip.To16() == nil {
			r = Range{
				Description: "bad IP",
			}
			ok = true
		}
		return
	}
	return lookup(me.getFirst, me.getRange, me.len(), ip4)
}
