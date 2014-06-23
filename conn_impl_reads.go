package enproxy

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// submitRead submits a read to the processReads goroutine, returning true if
// the read was accepted or false if reads are no longer being accepted
func (c *Conn) submitRead(b []byte) bool {
	c.readMutex.RLock()
	defer c.readMutex.RUnlock()
	if c.doneReading {
		return false
	} else {
		c.readRequestsCh <- b
		return true
	}
}

// processReads processes read requests by polling the proxy with GET requests
// and reading the data from the resulting response body
func (c *Conn) processReads() {
	var resp *http.Response

	defer c.cleanupAfterReads(resp)

	// Dial proxy
	proxyConn, bufReader, err := c.dialProxy()
	if err != nil {
		log.Printf("Unable to dial proxy to GET data: %s", err)
		return
	}
	defer func() {
		// If there's a proxyConn at the time that processReads() exits, close
		// it.
		if proxyConn != nil {
			proxyConn.Close()
		}
	}()

	// Wait for proxy host determined by first write request so that we know
	// where to send read requests.
	initialResponse := <-c.initialResponseCh
	if initialResponse.err != nil {
		return
	}
	proxyHost := initialResponse.proxyHost
	// Also grab the initial response body to save an extra round trip for the
	// first read
	resp = initialResponse.resp

	for {
		if c.isClosed() {
			return
		}

		select {
		case b := <-c.readRequestsCh:
			if resp == nil {
				// Old response finished
				if c.isIdle() {
					// We're idle, don't bother reading again
					return
				}

				// First, redial the proxy if necessary
				proxyConn, bufReader, err := c.redialProxyIfNecessary(proxyConn, bufReader)
				if err != nil {
					c.readResponsesCh <- rwResponse{0, fmt.Errorf("Unable to redial proxy: %s", err)}
				}

				// Then, issue a new request
				resp, err = c.doRequest(proxyConn, bufReader, proxyHost, OP_READ, nil)
				if err != nil {
					err = fmt.Errorf("Unable to issue read request: %s", err)
					log.Println(err.Error())
					c.readResponsesCh <- rwResponse{0, err}
					return
				}
			}

			// Process read, but don't wait longer than IdleTimeout
			proxyConn.SetReadDeadline(time.Now().Add(c.Config.IdleTimeout))
			n, err := resp.Body.Read(b)
			if n > 0 {
				c.markActive()
			}

			hitEOFUpstream := resp.Header.Get(X_ENPROXY_EOF) == "true"
			errToClient := err
			if err == io.EOF && !hitEOFUpstream {
				// The current response hit EOF, but we haven't hit EOF upstream
				// so suppress EOF to reader
				errToClient = nil
			}
			c.readResponsesCh <- rwResponse{n, errToClient}

			if err != nil {
				if err == io.EOF {
					// Current response is done
					resp.Body.Close()
					resp = nil
					if hitEOFUpstream {
						// True EOF, stop reading
						return
					}
					continue
				} else {
					log.Printf("Unexpected error reading from proxyConn: %s", err)
					return
				}
			}
		case <-c.stopReadCh:
			return
		case <-time.After(c.Config.IdleTimeout):
			if c.isIdle() {
				return
			}
		}
	}
	return
}

func (c *Conn) cleanupAfterReads(resp *http.Response) {
	c.readMutex.Lock()
	c.doneReading = true
	c.readMutex.Unlock()
	for {
		select {
		case <-c.readRequestsCh:
			c.readResponsesCh <- rwResponse{0, io.EOF}
		case <-c.stopReadCh:
			// do nothing
		default:
			close(c.readRequestsCh)
			if resp != nil {
				resp.Body.Close()
			}
			return
		}
	}
}
