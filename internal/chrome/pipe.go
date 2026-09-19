package chrome

import (
	"bufio"
	"os"
	"sync"
)

// pipeTransport speaks Chrome's --remote-debugging-pipe protocol: JSON
// messages separated by NUL bytes over fds 3 (Chrome reads) and 4 (Chrome
// writes). No framing, no TCP.
type pipeTransport struct {
	r  *bufio.Reader
	rf *os.File
	wf *os.File
	mu sync.Mutex
}

func newPipeTransport(fromChrome, toChrome *os.File) *pipeTransport {
	return &pipeTransport{r: bufio.NewReaderSize(fromChrome, 1<<20), rf: fromChrome, wf: toChrome}
}

func (p *pipeTransport) Read() ([]byte, error) {
	b, err := p.r.ReadBytes(0)
	if err != nil {
		return nil, err
	}
	return b[:len(b)-1], nil
}

func (p *pipeTransport) Write(d []byte) error {
	buf := make([]byte, len(d)+1)
	copy(buf, d)
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.wf.Write(buf)
	return err
}

func (p *pipeTransport) Close() error {
	e1 := p.wf.Close()
	e2 := p.rf.Close()
	if e1 != nil {
		return e1
	}
	return e2
}
