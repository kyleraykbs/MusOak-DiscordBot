package voice

import (
	"bytes"
	"io"
	"testing"
)

func collect(t *testing.T, r *oggReader, n int) [][]byte {
	t.Helper()
	var pkts [][]byte
	for range n {
		pkt, err := r.next()
		if err != nil {
			t.Fatalf("packet %d: %v", len(pkts), err)
		}
		pkts = append(pkts, pkt)
	}
	return pkts
}

func TestPacketsFromOnePage(t *testing.T) {
	want := [][]byte{[]byte("first"), []byte("second packet"), []byte("third")}
	stream := buildPage(want, false)
	r := newOggReader(bytes.NewReader(stream))
	got := collect(t, r, len(want))
	for i, pkt := range want {
		if !bytes.Equal(got[i], pkt) {
			t.Errorf("packet %d = %q, want %q", i, got[i], pkt)
		}
	}
	if _, err := r.next(); err != io.EOF {
		t.Errorf("after the last packet: %v, want EOF", err)
	}
}

func TestJunkBeforeTheFirstPageIsSkipped(t *testing.T) {
	want := [][]byte{[]byte("payload")}
	stream := append([]byte("not an ogg stream at allOggXnope"), buildPage(want, false)...)
	r := newOggReader(bytes.NewReader(stream))
	got := collect(t, r, 1)
	if !bytes.Equal(got[0], want[0]) {
		t.Errorf("packet = %q, want %q", got[0], want[0])
	}
}

func TestPacketSpanningTwoPages(t *testing.T) {
	pkt := bytes.Repeat([]byte{0xAB}, 600) // 600 = 255 + 255 + 90
	head, tail := splitPacket(pkt, 510)
	stream := append(append([]byte{}, head...), tail...)

	r := newOggReader(bytes.NewReader(stream))
	got, err := r.next()
	if err != nil {
		t.Fatalf("spanning packet: %v", err)
	}
	if !bytes.Equal(got, pkt) {
		t.Errorf("spanning packet came back as %d bytes, want %d (equal: %v)",
			len(got), len(pkt), bytes.Equal(got, pkt))
	}
}

func TestAnUnfinishedPacketAtEndOfStreamIsDropped(t *testing.T) {
	pkt := bytes.Repeat([]byte{0xCD}, 400)
	head, _ := splitPacket(pkt, 255)
	r := newOggReader(bytes.NewReader(head))
	if _, err := r.next(); err != io.EOF {
		t.Errorf("half a packet returned as a packet or failed: %v, want EOF", err)
	}
}

func TestAPageEndingIn255LacesCarriesThePacketOn(t *testing.T) {
	// Two pages: first holds packets A and the head of B (as 255-laces with
	// no terminator), second continues B and holds C.
	pktB := bytes.Repeat([]byte{0x11}, 300)
	stream := buildRaw(
		append(lace(1), byte(255)), // A terminates, then the head of B continues
		append([]byte{'A'}, pktB[:255]...),
		false,
	)
	stream = append(stream, buildPage([][]byte{pktB[255:], []byte("C")}, true)...)

	r := newOggReader(bytes.NewReader(stream))
	got := collect(t, r, 3)
	if string(got[0]) != "A" {
		t.Errorf("first packet = %q, want A", got[0])
	}
	if !bytes.Equal(got[1], pktB) {
		t.Errorf("second packet = %d bytes of %x..., want the whole B", len(got[1]), got[1][:4])
	}
	if string(got[2]) != "C" {
		t.Errorf("third packet = %q, want C", got[2])
	}
}

func lace(n int) []byte {
	if n >= 255 {
		panic("lace only builds small terminators")
	}
	return []byte{byte(n)}
}
