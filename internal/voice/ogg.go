// Ogg page parsing for the sink: it splits what ffmpeg writes on stdout into
// the opus packets the voice connection takes, one packet at a time.
package voice

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// oggSync is the page capture pattern.
var oggSync = []byte("OggS")

// oggReader splits an Ogg byte stream into packets. It ignores serial numbers,
// granule positions and CRCs: the bytes come from ffmpeg, whose framing is
// sound, and the reader only has to agree with it about where packets end.
type oggReader struct {
	r       *bufio.Reader
	partial []byte   // a packet still being assembled across pages
	queue   [][]byte // packets completed by the last page
	// err is the end of the stream, remembered: a caller that peeks past the
	// last packet (a run being warmed, say) must not turn the run's clean end
	// into a closed-pipe error for the next caller.
	err error
}

func newOggReader(r io.Reader) *oggReader {
	return &oggReader{r: bufio.NewReaderSize(r, 64<<10)}
}

// next returns the next packet, or io.EOF when the stream does - the same
// error, on every call after the stream has ended.
func (o *oggReader) next() ([]byte, error) {
	if o.err != nil {
		return nil, o.err
	}
	for len(o.queue) == 0 {
		if err := o.readPage(); err != nil {
			o.err = err
			return nil, err
		}
	}
	pkt := o.queue[0]
	o.queue = o.queue[1:]
	return pkt, nil
}

// ready reports whether anything more is already in hand: a packet queued from
// the last page, or bytes of the next one already read off the pipe. It is how
// a caller fills a buffer with what a producer has already written, without
// waiting for more that may not come for a while.
func (o *oggReader) ready() bool {
	return len(o.queue) > 0 || o.r.Buffered() > 0
}

// readPage finds the next page and turns its lacing values into packets.
func (o *oggReader) readPage() error {
	if err := o.findSync(); err != nil {
		return err
	}
	// version, header type, granule, serial, sequence, crc, segment count
	header := make([]byte, 23)
	if _, err := io.ReadFull(o.r, header); err != nil {
		return err
	}
	if header[0] != 0 {
		return errors.New("ogg: unknown version")
	}
	continued := header[1]&0x01 != 0
	nsegs := int(header[22])

	if !continued {
		// The page starts fresh: whatever was being assembled is a packet the
		// stream never finished. Drop it rather than emit a corrupt one.
		o.partial = o.partial[:0]
	}

	laces := make([]byte, nsegs)
	if _, err := io.ReadFull(o.r, laces); err != nil {
		return err
	}
	body := 0
	for _, l := range laces {
		body += int(l)
	}
	payload := make([]byte, body)
	if _, err := io.ReadFull(o.r, payload); err != nil {
		return err
	}

	at := 0
	for _, l := range laces {
		o.partial = append(o.partial, payload[at:at+int(l)]...)
		at += int(l)
		if l < 255 {
			// A short lacing value terminates a packet.
			o.queue = append(o.queue, o.partial)
			o.partial = make([]byte, 0, len(o.partial))
		}
	}
	return nil
}

// findSync consumes bytes up to and including the next "OggS". Bytes that
// turn out not to start a page are dropped, which costs nothing: everything
// before the first page is garbage by definition.
func (o *oggReader) findSync() error {
	var window []byte
	for {
		b, err := o.r.ReadByte()
		if err != nil {
			return err
		}
		window = append(window, b)
		if len(window) > len(oggSync) {
			window = window[1:]
		}
		if len(window) == len(oggSync) && bytes.Equal(window, oggSync) {
			return nil
		}
	}
}

// buildPage frames packets into one Ogg page for tests. The lacing is chosen
// the way a muxer would: 255-byte continuations for packets longer than 255,
// a short lace to terminate each. The CRC is left zero; nothing here checks
// it. continued sets the page's continuation flag, for the second half of a
// packet split across pages.
func buildPage(packets [][]byte, continued bool) []byte {
	var laces []byte
	var body []byte
	for _, pkt := range packets {
		rest := pkt
		for len(rest) >= 255 {
			laces = append(laces, 255)
			body = append(body, rest[:255]...)
			rest = rest[255:]
		}
		laces = append(laces, byte(len(rest)))
		body = append(body, rest...)
	}
	return buildRaw(laces, body, continued)
}

// buildRaw writes a page from a pre-built lacing table and body.
func buildRaw(laces, body []byte, continued bool) []byte {
	page := make([]byte, 0, 27+len(laces)+len(body))
	page = append(page, oggSync...)
	page = append(page, 0) // version
	htype := byte(0)
	if continued {
		htype |= 0x01
	}
	page = append(page, htype)
	var zeros [20]byte // granule (8) + serial (4) + sequence (4) + crc (4)
	page = append(page, zeros[:]...)
	page = append(page, byte(len(laces)))
	page = append(page, laces...)
	page = append(page, body...)
	return page
}

// splitPacket cuts a packet so it spans two pages. at must land on a 255
// boundary inside the packet: a page that ends mid-packet carries only
// 255-laces, which say "continues on the next page".
func splitPacket(pkt []byte, at int) (head, tail []byte) {
	if at <= 0 || at >= len(pkt) || at%255 != 0 {
		panic("voice: splitPacket needs a 255-boundary inside the packet")
	}
	laces := make([]byte, at/255)
	for i := range laces {
		laces[i] = 255
	}
	head = buildRaw(laces, pkt[:at], false)
	tail = buildPage([][]byte{pkt[at:]}, true)
	return head, tail
}
