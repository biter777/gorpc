package gorpc

import (
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Client implements RPC client.
//
// The client must be started with Client.Start() before use.
//
// It is absolutely safe and encouraged using a single client across arbitrary
// number of concurrently running goroutines.
//
// Default client settings are optimized for high load, so don't override
// them without valid reason.
type Client struct {
	// Server address to connect to.
	//
	// The address format depends on the underlying transport provided
	// by Client.Dial. The following transports are provided out of the box:
	//   * TCP - see NewTCPClient() and NewTCPServer().
	//   * TLS - see NewTLSClient() and NewTLSServer().
	//   * Unix sockets - see NewUnixClient() and NewUnixServer().
	//
	// By default TCP transport is used.
	Addr string

	// The number of concurrent connections the client should establish
	// to the sever.
	// By default only one connection is established.
	Conns int

	// The maximum number of pending requests in the queue.
	//
	// The number of pending requsts should exceed the expected number
	// of concurrent goroutines calling client's methods.
	// Otherwise a lot of ClientError.Overflow errors may appear.
	//
	// Default is DefaultPendingMessages.
	PendingRequests int

	// Delay between request flushes.
	//
	// Negative values lead to immediate requests' sending to the server
	// without their buffering. This minimizes rpc latency at the cost
	// of higher CPU and network usage.
	//
	// Default value is DefaultFlushDelay.
	FlushDelay time.Duration

	// Maximum request time.
	// Default value is DefaultRequestTimeout.
	RequestTimeout time.Duration

	// Disable data compression.
	// By default data compression is enabled.
	DisableCompression bool

	// Size of send buffer per each underlying connection in bytes.
	// Default value is DefaultBufferSize.
	SendBufferSize int

	// Size of recv buffer per each underlying connection in bytes.
	// Default value is DefaultBufferSize.
	RecvBufferSize int

	// OnConnect is called whenever connection to server is established.
	// The callback can be used for authentication/authorization/encryption
	// and/or for custom transport wrapping.
	//
	// See also Dial callback, which can be used for sophisticated
	// transport implementation.
	OnConnect OnConnectFunc

	// The client calls this callback when it needs new connection
	// to the server.
	// The client passes Client.Addr into Dial().
	//
	// Override this callback if you want custom underlying transport
	// and/or authentication/authorization.
	// Don't forget overriding Server.Listener accordingly.
	//
	// See also OnConnect for authentication/authorization purposes.
	//
	// * NewTLSClient() and NewTLSServer() can be used for encrypted rpc.
	// * NewUnixClient() and NewUnixServer() can be used for fast local
	//   inter-process rpc.
	//
	// By default it returns TCP connections established to the Client.Addr.
	Dial DialFunc

	// LogError is used for error logging.
	//
	// By default the function set via SetErrorLogger() is used.
	LogError LoggerFunc

	// Connection statistics.
	//
	// The stats doesn't reset automatically. Feel free resetting it
	// any time you wish.
	Stats ConnStats

	pendingRequestsCount uint32
	requestsChan         chan *AsyncResult

	clientStopChan chan struct{}
	stopWg         sync.WaitGroup
}

// Start starts rpc client. Establishes connection to the server on Client.Addr.
//
// All the request and response types the client may use must be registered
// via RegisterType() before starting the client.
// There is no need in registering base Go types such as int, string, bool,
// float64, etc. or arrays, slices and maps containing base Go types.
func (c *Client) Start() {
	if c.LogError == nil {
		c.LogError = errorLogger
	}
	if c.clientStopChan != nil {
		panic("gorpc.Client: the given client is already started. Call Client.Stop() before calling Client.Start() again!")
	}

	if c.PendingRequests <= 0 {
		c.PendingRequests = DefaultPendingMessages
	}
	if c.FlushDelay == 0 {
		c.FlushDelay = DefaultFlushDelay
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = DefaultRequestTimeout
	}
	if c.SendBufferSize <= 0 {
		c.SendBufferSize = DefaultBufferSize
	}
	if c.RecvBufferSize <= 0 {
		c.RecvBufferSize = DefaultBufferSize
	}

	c.requestsChan = make(chan *AsyncResult, c.PendingRequests)
	c.clientStopChan = make(chan struct{})

	if c.Conns <= 0 {
		c.Conns = 1
	}
	if c.Dial == nil {
		c.Dial = defaultDial
	}

	for i := 0; i < c.Conns; i++ {
		c.stopWg.Add(1)
		go clientHandler(c)
	}
}

// Stop stops rpc client. Stopped client can be started again.
func (c *Client) Stop() {
	if c.clientStopChan == nil {
		panic("gorpc.Client: the client must be started before stopping it")
	}
	close(c.clientStopChan)
	c.stopWg.Wait()
	c.clientStopChan = nil
}

// PendingRequestsCount returns the instant number of pending requests.
//
// The main purpose of this function is to use in load-balancing schemes where
// load should be balanced between multiple rpc clients.
//
// Don't forget starting the client with Client.Start() before calling
// this function.
func (c *Client) PendingRequestsCount() int {
	n := atomic.LoadUint32(&c.pendingRequestsCount)
	return int(n) + len(c.requestsChan)
}

// Call sends the given request to the server and obtains response
// from the server.
// Returns non-nil error if the response cannot be obtained during
// Client.RequestTimeout or server connection problems occur.
// The returned error can be casted to ClientError.
//
// Request and response types may be arbitrary. All the request and response
// types the client may use must be registered via RegisterType() before
// starting the client.
// There is no need in registering base Go types such as int, string, bool,
// float64, etc. or arrays, slices and maps containing base Go types.
//
// Hint: use Dispatcher for distinct calls' construction.
//
// Don't forget starting the client with Client.Start() before calling Client.Call().
func (c *Client) Call(request interface{}) (response interface{}, err error) {
	return c.CallTimeout(request, c.RequestTimeout)
}

// CallTimeout sends the given request to the server and obtains response
// from the server.
// Returns non-nil error if the response cannot be obtained during
// the given timeout or server connection problems occur.
// The returned error can be casted to ClientError.
//
// Request and response types may be arbitrary. All the request and response
// types the client may use must be registered via RegisterType() before
// starting the client.
// There is no need in registering base Go types such as int, string, bool,
// float64, etc. or arrays, slices and maps containing base Go types.
//
// Hint: use Dispatcher for distinct calls' construction.
//
// Don't forget starting the client with Client.Start() before calling Client.Call().
func (c *Client) CallTimeout(request interface{}, timeout time.Duration) (response interface{}, err error) {
	var m *AsyncResult
	if m, err = c.callAsync(request, false, true); err != nil {
		return nil, err
	}

	t := acquireTimer(timeout)

	select {
	case <-m.Done:
		response, err = m.Response, m.Error
		releaseAsyncResult(m)
	case <-t.C:
		m.Cancel()
		err = getClientTimeoutError(c, timeout)
	}

	releaseTimer(t)
	return
}

func acquireAsyncResult() *AsyncResult {
	v := asyncResultPool.Get()
	if v == nil {
		return &AsyncResult{}
	}
	return v.(*AsyncResult)
}

var zeroTime time.Time

func releaseAsyncResult(m *AsyncResult) {
	m.Response = nil
	m.Error = nil
	m.Done = nil
	m.request = nil
	m.t = zeroTime
	m.done = nil
	asyncResultPool.Put(m)
}

var asyncResultPool sync.Pool

func getClientTimeoutError(c *Client, timeout time.Duration) error {
	err := fmt.Errorf("gorpc.Client: [%s]. Cannot obtain response during timeout=%s", c.Addr, timeout)
	c.LogError("%s", err)
	return &ClientError{
		Timeout: true,
		err:     err,
	}
}

// Send sends the given request to the server and doesn't wait for response.
//
// Since this is 'fire and forget' function, which never waits for response,
// it cannot guarantee that the server receives and successfully processes
// the given request. Though in most cases under normal conditions requests
// should reach the server and it should successfully process them.
// Send semantics is similar to UDP messages' semantics.
//
// The server may return arbitrary response on Send() request, but the response
// is totally ignored.
//
// All the request types the client may use must be registered
// via RegisterType() before starting the client.
// There is no need in registering base Go types such as int, string, bool,
// float64, etc. or arrays, slices and maps containing base Go types.
//
// Don't forget starting the client with Client.Start() before calling Client.Send().
func (c *Client) Send(request interface{}) error {
	_, err := c.callAsync(request, true, true)
	return err
}

// AsyncResult is a result returned from Client.CallAsync().
type AsyncResult struct {
	// The response can be read only after <-Done unblocks.
	Response interface{}

	// The error can be read only after <-Done unblocks.
	// The error can be casted to ClientError.
	Error error

	// Response and Error become available after <-Done unblocks.
	Done <-chan struct{}

	request  interface{}
	t        time.Time
	done     chan struct{}
	canceled uint32
}

// Cancel cancels async call.
//
// Canceled call isn't sent to the server unless it is already sent there.
// Canceled call may successfully complete if it has been already sent
// to the server before Cancel call.
//
// It is safe calling this function multiple times from concurrently
// running goroutines.
func (m *AsyncResult) Cancel() {
	atomic.StoreUint32(&m.canceled, 1)
}

func (m *AsyncResult) isCanceled() bool {
	return atomic.LoadUint32(&m.canceled) != 0
}

// CallAsync starts async rpc call.
//
// Rpc call is complete after <-AsyncResult.Done unblocks.
// If you want canceling the request, just throw away the returned AsyncResult.
//
// CallAsync doesn't respect Client.RequestTimeout - response timeout
// may be controlled by the caller via something like:
//
//     r := c.CallAsync("foobar")
//     select {
//     case <-time.After(c.RequestTimeout):
//        log.Printf("rpc timeout!")
//     case <-r.Done:
//        processResponse(r.Response, r.Error)
//     }
//
// Request and response types may be arbitrary. All the request and response
// types the client may use must be registered via RegisterType() before
// starting the client.
// There is no need in registering base Go types such as int, string, bool,
// float64, etc. or arrays, slices and maps containing base Go types.
//
// Don't forget starting the client with Client.Start() before
// calling Client.CallAsync().
func (c *Client) CallAsync(request interface{}) (*AsyncResult, error) {
	return c.callAsync(request, false, false)
}

func (c *Client) callAsync(request interface{}, skipResponse bool, usePool bool) (m *AsyncResult, err error) {
	if skipResponse {
		usePool = true
	}

	if usePool {
		m = acquireAsyncResult()
	} else {
		m = &AsyncResult{}
	}
	m.request = request
	if !skipResponse {
		m.t = time.Now()
		m.done = make(chan struct{})
		m.Done = m.done
	}

	select {
	case c.requestsChan <- m:
		return m, nil
	default:
		// Try substituting the oldest async request by the new one
		// on requests' queue overflow.
		// This increases the chances for new request to succeed
		// without timeout.
		if skipResponse {
			// Immediately notify the caller not interested
			// in the response on requests' queue overflow, since
			// there are no other ways to notify it later.
			releaseAsyncResult(m)
			return nil, overflowClientError(c)
		}

		select {
		case mm := <-c.requestsChan:
			if mm.done != nil {
				mm.Error = overflowClientError(c)
				close(mm.done)
			} else {
				releaseAsyncResult(mm)
			}
		default:
		}

		select {
		case c.requestsChan <- m:
			return m, nil
		default:
			// Release m even if usePool = true, since m wasn't exposed
			// to the caller yet.
			releaseAsyncResult(m)
			return nil, overflowClientError(c)
		}
	}
}

func overflowClientError(c *Client) error {
	err := fmt.Errorf("gorpc.Client: [%s]. Requests' queue with size=%d is overflown. Try increasing Client.PendingRequests value", c.Addr, cap(c.requestsChan))
	c.LogError("%s", err)
	return &ClientError{
		Overflow: true,
		err: fmt.Errorf("gorpc.Client: [%s]. Requests' queue with size=%d is overflown. "+
			"Try increasing Client.PendingRequests value", c.Addr, cap(c.requestsChan)),
	}
}

// Batch allows grouping and executing multiple RPCs in a single batch.
//
// Batch may be created via Client.NewBatch().
type Batch struct {
	c       *Client
	ops     []*BatchResult
	opsLock sync.Mutex
}

// BatchResult is a result returned from Batch.Add*().
type BatchResult struct {
	// The response can be read only after Batch.Call*() returns.
	Response interface{}

	// The error can be read only after Batch.Call*() returns.
	// The error can be casted to ClientError.
	Error error

	// <-Done unblocks after Batch.Call*() returns.
	// Response and Error become available after <-Done unblocks.
	Done <-chan struct{}

	request interface{}
	ctx     interface{}
	done    chan struct{}
}

// NewBatch creates new RPC batch.
//
// It is safe creating multiple concurrent batches from a single client.
//
// Don't forget starting the client with Client.Start() before working
// with batched RPC.
func (c *Client) NewBatch() *Batch {
	return &Batch{
		c: c,
	}
}

// Add ads new request to the RPC batch.
//
// The order of batched RPCs execution on the server is unspecified.
//
// All the requests added to the batch are sent to the server at once
// when Batch.Call*() is called.
//
// Request and response types may be arbitrary. All the request and response
// types the client may use must be registered via RegisterType() before
// starting the client.
// There is no need in registering base Go types such as int, string, bool,
// float64, etc. or arrays, slices and maps containing base Go types.
//
// It is safe adding multiple requests to the same batch from concurrently
// running goroutines.
func (b *Batch) Add(request interface{}) *BatchResult {
	return b.add(request, false)
}

// AddSkipResponse adds new request to the RPC batch and doesn't care
// about the response.
//
// The order of batched RPCs execution on the server is unspecified.
//
// All the requests added to the batch are sent to the server at once
// when Batch.Call*() is called.
//
// All the request types the client may use must be registered
// via RegisterType() before starting the client.
// There is no need in registering base Go types such as int, string, bool,
// float64, etc. or arrays, slices and maps containing base Go types.
//
// It is safe adding multiple requests to the same batch from concurrently
// running goroutines.
func (b *Batch) AddSkipResponse(request interface{}) {
	b.add(request, true)
}

func (b *Batch) add(request interface{}, skipResponse bool) *BatchResult {
	br := &BatchResult{
		request: request,
	}
	if !skipResponse {
		br.done = make(chan struct{})
		br.Done = br.done
	}

	b.opsLock.Lock()
	b.ops = append(b.ops, br)
	b.opsLock.Unlock()

	return br
}

// Call calls all the RPCs added via Batch.Add().
//
// The order of batched RPCs execution on the server is unspecified.
// Usually batched RPCs are executed concurrently on the server.
//
// The caller may read all BatchResult contents returned from Batch.Add()
// after the Call returns.
//
// It is guaranteed that all <-BatchResult.Done channels are unblocked after
// the Call returns.
func (b *Batch) Call() error {
	return b.CallTimeout(b.c.RequestTimeout)
}

// CallTimeout calls all the RPCs added via Batch.Add() and waits for
// all the RPC responses during the given timeout.
//
// The order of batched RPCs execution on the server is unspecified.
// Usually batched RPCs are executed concurrently on the server.
//
// The caller may read all BatchResult contents returned from Batch.Add()
// after the CallTimeout returns.
//
// It is guaranteed that all <-BatchResult.Done channels are unblocked after
// the CallTimeout returns.
func (b *Batch) CallTimeout(timeout time.Duration) error {
	b.opsLock.Lock()
	ops := b.ops
	b.ops = nil
	b.opsLock.Unlock()

	results := make([]*AsyncResult, len(ops))
	for i := range ops {
		op := ops[i]
		m, err := callAsyncRetry(b.c, op.request, op.done == nil, 5)
		if err != nil {
			return err
		}
		results[i] = m
	}

	t := acquireTimer(timeout)

	for i := range results {
		m := results[i]
		op := ops[i]
		if op.done == nil {
			continue
		}

		select {
		case <-m.Done:
			op.Response, op.Error = m.Response, m.Error
			close(op.done)
		case <-t.C:
			releaseTimer(t)
			err := getClientTimeoutError(b.c, timeout)
			for ; i < len(results); i++ {
				results[i].Cancel()
				op = ops[i]
				op.Error = err
				if op.done != nil {
					close(op.done)
				}
			}
			return err
		}
	}

	releaseTimer(t)

	return nil
}

func callAsyncRetry(c *Client, request interface{}, skipResponse bool, retriesCount int) (*AsyncResult, error) {
	retriesCount++
	for {
		m, err := c.callAsync(request, skipResponse, false)
		if err == nil {
			return m, nil
		}
		if !err.(*ClientError).Overflow {
			return nil, err
		}
		retriesCount--
		if retriesCount <= 0 {
			return nil, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ClientError is an error Client methods can return.
type ClientError struct {
	// Set if the error is timeout-related.
	Timeout bool

	// Set if the error is connection-related.
	Connection bool

	// Set if the error is server-related.
	Server bool

	// Set if the error is related to internal resources' overflow.
	// Increase PendingRequests if you see a lot of such errors.
	Overflow bool

	// May be set if AsyncResult.Cancel is called.
	Canceled bool

	err error
}

func (e *ClientError) Error() string {
	return e.err.Error()
}

// ErrCanceled may be returned from rpc call if AsyncResult.Cancel
// has been called.
var ErrCanceled = &ClientError{
	Canceled: true,
	err:      fmt.Errorf("the call has been canceled"),
}

func clientHandler(c *Client) {
	defer c.stopWg.Done()

	var conn io.ReadWriteCloser
	var err error
	var stopping atomic.Value

	for {
		dialChan := make(chan struct{})
		go func() {
			if conn, err = c.Dial(c.Addr); err != nil {
				if stopping.Load() == nil {
					c.LogError("gorpc.Client: [%s]. Cannot establish rpc connection: [%s]", c.Addr, err)
				}
			}
			close(dialChan)
		}()

		select {
		case <-c.clientStopChan:
			stopping.Store(true)
			<-dialChan
			return
		case <-dialChan:
			c.Stats.incDialCalls()
		}

		if err != nil {
			c.Stats.incDialErrors()
			select {
			case <-c.clientStopChan:
				return
			case <-time.After(time.Second):
			}
			continue
		}

		clientHandleConnection(c, conn)

		select {
		case <-c.clientStopChan:
			return
		default:
		}
	}
}

func clientHandleConnection(c *Client, conn io.ReadWriteCloser) {
	if c.OnConnect != nil {
		newConn, err := c.OnConnect(c.Addr, conn)
		if err != nil {
			c.LogError("gorpc.Client: [%s]. OnConnect error: [%s]", c.Addr, err)
			if conn != nil {
				conn.Close()
			}
			return
		}
		conn = newConn
	}

	var buf [1]byte
	if !c.DisableCompression {
		buf[0] = 1
	}
	_, err := conn.Write(buf[:])
	if err != nil {
		c.LogError("gorpc.Client: [%s]. Error when writing handshake to server: [%s]", c.Addr, err)
		conn.Close()
		return
	}

	stopChan := make(chan struct{})

	pendingRequests := make(map[uint64]*AsyncResult)
	var pendingRequestsLock sync.Mutex

	writerDone := make(chan error, 1)
	go clientWriter(c, conn, pendingRequests, &pendingRequestsLock, stopChan, writerDone)

	readerDone := make(chan error, 1)
	go clientReader(c, conn, pendingRequests, &pendingRequestsLock, readerDone)

	select {
	case err = <-writerDone:
		close(stopChan)
		conn.Close()
		<-readerDone
	case err = <-readerDone:
		close(stopChan)
		conn.Close()
		<-writerDone
	case <-c.clientStopChan:
		close(stopChan)
		conn.Close()
		<-readerDone
		<-writerDone
	}

	if err != nil {
		c.LogError("%s", err)
		err = &ClientError{
			Connection: true,
			err:        err,
		}
	}
	for _, m := range pendingRequests {
		atomic.AddUint32(&c.pendingRequestsCount, ^uint32(0))
		m.Error = err
		if m.done != nil {
			close(m.done)
		}
	}
}

func clientWriter(c *Client, w io.Writer, pendingRequests map[uint64]*AsyncResult, pendingRequestsLock *sync.Mutex, stopChan <-chan struct{}, done chan<- error) {
	var err error
	defer func() { done <- err }()

	e := newMessageEncoder(w, c.SendBufferSize, !c.DisableCompression, &c.Stats)
	defer e.Close()

	t := time.NewTimer(c.FlushDelay)
	var flushChan <-chan time.Time
	var wr wireRequest
	var msgID uint64
	for {
		var m *AsyncResult

		select {
		case m = <-c.requestsChan:
		default:
			// Give the last chance for ready goroutines filling c.requestsChan :)
			runtime.Gosched()

			select {
			case <-stopChan:
				return
			case m = <-c.requestsChan:
			case <-flushChan:
				if err = e.Flush(); err != nil {
					err = fmt.Errorf("gorpc.Client: [%s]. Cannot flush requests to underlying stream: [%s]", c.Addr, err)
					return
				}
				flushChan = nil
				continue
			}
		}

		if flushChan == nil {
			flushChan = getFlushChan(t, c.FlushDelay)
		}

		if m.isCanceled() {
			if m.done != nil {
				m.Error = ErrCanceled
				close(m.done)
			} else {
				releaseAsyncResult(m)
			}
			continue
		}

		if m.done == nil {
			wr.ID = 0
		} else {
			msgID++
			if msgID == 0 {
				msgID = 1
			}
			pendingRequestsLock.Lock()
			n := len(pendingRequests)
			for {
				if _, ok := pendingRequests[msgID]; !ok {
					break
				}
				msgID++
			}
			pendingRequests[msgID] = m
			pendingRequestsLock.Unlock()
			atomic.AddUint32(&c.pendingRequestsCount, 1)

			if n > 10*c.PendingRequests {
				err = fmt.Errorf("gorpc.Client: [%s]. The server didn't return %d responses yet. Closing server connection in order to prevent client resource leaks", c.Addr, n)
				return
			}

			wr.ID = msgID
		}

		wr.Request = m.request
		if m.done == nil {
			c.Stats.incRPCCalls()
			releaseAsyncResult(m)
		}

		if err = e.Encode(wr); err != nil {
			err = fmt.Errorf("gorpc.Client: [%s]. Cannot send request to wire: [%s]", c.Addr, err)
			return
		}
		wr.Request = nil
	}
}

func clientReader(c *Client, r io.Reader, pendingRequests map[uint64]*AsyncResult, pendingRequestsLock *sync.Mutex, done chan<- error) {
	var err error
	defer func() {
		if r := recover(); r != nil {
			if err == nil {
				err = fmt.Errorf("gorpc.Client: [%s]. Panic when reading data from server: %v", c.Addr, r)
			}
		}
		done <- err
	}()

	d := newMessageDecoder(r, c.RecvBufferSize, !c.DisableCompression, &c.Stats)
	defer d.Close()

	var wr wireResponse
	for {
		if err = d.Decode(&wr); err != nil {
			err = fmt.Errorf("gorpc.Client: [%s]. Cannot decode response: [%s]", c.Addr, err)
			return
		}

		pendingRequestsLock.Lock()
		m, ok := pendingRequests[wr.ID]
		if ok {
			delete(pendingRequests, wr.ID)
		}
		pendingRequestsLock.Unlock()

		if !ok {
			err = fmt.Errorf("gorpc.Client: [%s]. Unexpected msgID=[%d] obtained from server", c.Addr, wr.ID)
			return
		}

		atomic.AddUint32(&c.pendingRequestsCount, ^uint32(0))

		m.Response = wr.Response

		wr.ID = 0
		wr.Response = nil
		if wr.Error != "" {
			m.Error = &ClientError{
				Server: true,
				err:    fmt.Errorf("gorpc.Client: [%s]. Server error: [%s]", c.Addr, wr.Error),
			}
			wr.Error = ""
		}

		c.Stats.incRPCCalls()
		c.Stats.incRPCTime(uint64(time.Since(m.t).Seconds() * 1000))

		close(m.done)
	}
}
