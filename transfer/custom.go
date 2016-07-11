package transfer

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"

	"github.com/github/git-lfs/api"
	"github.com/github/git-lfs/subprocess"
	"github.com/rubyist/tracerx"

	"github.com/github/git-lfs/config"
)

// Adapter for custom transfer via external process
type customAdapter struct {
	*adapterBase
	path                string
	args                string
	concurrent          bool
	originalConcurrency int
}

type customAdapterWorkerContext struct {
	cmd         *exec.Cmd
	stdout      io.ReadCloser
	bufferedOut *bufio.Reader
	stdin       io.WriteCloser
}

type customAdapterInitRequest struct {
	Operation           string `json:"operation"`
	Concurrent          bool   `json:"concurrent"`
	ConcurrentTransfers int    `json:"concurrenttransfers"`
}
type customAdapterInitResponse struct {
	Error *api.ObjectError `json:"error,omitempty"`
}
type customAdapterUploadRequest struct {
	Oid    string            `json:"oid"`
	Size   int64             `json:"size"`
	Path   string            `json:"path"`
	Action *api.LinkRelation `json:"action"`
}
type customAdapterUploadResponse struct {
	Oid   string           `json:"oid"`
	Error *api.ObjectError `json:"error,omitempty"`
}
type customAdapterDownloadRequest struct {
	Oid    string            `json:"oid"`
	Size   int64             `json:"size"`
	Action *api.LinkRelation `json:"action"`
}
type customAdapterDownloadResponse struct {
	Oid   string           `json:"oid"`
	Path  string           `json:"path,omitempty"`
	Error *api.ObjectError `json:"error,omitempty"`
}
type customAdapterTerminateRequest struct {
	Complete bool `json:"complete"`
}

func (a *customAdapter) Begin(maxConcurrency int, cb TransferProgressCallback, completion chan TransferResult) error {
	// If config says not to launch multiple processes, downgrade incoming value
	useConcurrency := maxConcurrency
	if !a.concurrent {
		useConcurrency = 1
	}
	a.originalConcurrency = maxConcurrency

	tracerx.Printf("xfer: Custom transfer adapter %q using concurrency %d", a.name, useConcurrency)

	// Use common workers impl, but downgrade workers to number of processes
	return a.adapterBase.Begin(useConcurrency, cb, completion)
}

func (a *customAdapter) ClearTempStorage() error {
	// no action requred
	return nil
}

func (a *customAdapter) WorkerStarting(workerNum int) (interface{}, error) {

	// Start a process per worker
	// If concurrent = false we have already dialled back workers to 1
	tracerx.Printf("xfer: starting up custom transfer process %q for worker %d", a.name, workerNum)
	cmd := subprocess.ExecCommand(a.path, a.args)
	outp, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("Failed to get stdout for custom transfer command %q remote: %v", a.path, err)
	}
	inp, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("Failed to get stdin for custom transfer command %q remote: %v", a.path, err)
	}
	err = cmd.Start()
	if err != nil {
		return nil, fmt.Errorf("Failed to start custom transfer command %q remote: %v", a.path, err)
	}
	// Set up buffered reader/writer since we operate on lines
	ctx := &customAdapterWorkerContext{cmd, outp, bufio.NewReader(outp), inp}

	// send initiate message
	op := "upload"
	if a.direction == Download {
		op = "download"
	}
	initReq := &customAdapterInitRequest{op, a.concurrent, a.originalConcurrency}
	var initResp customAdapterInitResponse
	err = a.exchangeMessage(ctx, initReq, &initResp)
	if err != nil {
		a.abortWorkerProcess(ctx)
		return nil, err
	}

	tracerx.Printf("xfer: %q for worker %d started OK", a.name, workerNum)

	// Save this process context and use in future callbacks
	return ctx, nil
}

// exchangeMessage sends a message to a process and reads a response if resp != nil
// Only fatal errors to communicate return an error, errors may be embedded in reply
func (a *customAdapter) exchangeMessage(ctx *customAdapterWorkerContext, req, resp interface{}) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	// Line oriented JSON
	b = append(b, '\n')
	_, err = ctx.stdin.Write(b)
	if err != nil {
		return err
	}
	// Read response if needed
	if resp != nil {
		line, err := ctx.bufferedOut.ReadString('\n')
		if err != nil {
			return err
		}
		return json.Unmarshal([]byte(line), resp)
	}
	return nil
}

// shutdownWorkerProcess terminates gracefully a custom adapter process
// returns an error if it couldn't shut down gracefully (caller may abortWorkerProcess)
func (a *customAdapter) shutdownWorkerProcess(ctx *customAdapterWorkerContext) error {
	termReq := &customAdapterTerminateRequest{true}
	err := a.exchangeMessage(ctx, termReq, nil)
	if err != nil {
		return err
	}
	return ctx.cmd.Wait()
}

// abortWorkerProcess terminates & aborts untidily, most probably breakdown of comms or internal error
func (a *customAdapter) abortWorkerProcess(ctx *customAdapterWorkerContext) {
	ctx.stdin.Close()
	ctx.stdout.Close()
	ctx.cmd.Process.Kill()
	ctx.cmd = nil

}
func (a *customAdapter) WorkerEnding(workerNum int, ctx interface{}) {
	customCtx, ok := ctx.(*customAdapterWorkerContext)
	if !ok {
		tracerx.Printf("Context object for custom transfer %q was of the wrong type", a.name)
		return
	}

	err := a.shutdownWorkerProcess(customCtx)
	if err != nil {
		tracerx.Printf("xfer: error finishing up custom transfer process %q, aborting: %v", a.name, err)
		a.abortWorkerProcess(customCtx)
	}
}

func (a *customAdapter) DoTransfer(ctx interface{}, t *Transfer, cb TransferProgressCallback, authOkFunc func()) error {
	if ctx == nil {
		return fmt.Errorf("Custom transfer %q was not properly initialized, see previous errors", a.name)
	}
	// TODO
	// customCtx, ok := ctx.(*customAdapterWorkerContext)
	// if !ok {
	// 	return fmt.Errorf("Context object for custom transfer %q was of the wrong type", a.name)
	// }

	// TODO call authOK on first non-zero progress
	if authOkFunc != nil {
		authOkFunc()
	}

	// TODO send transfer request, receive progress and completion
	if cb != nil {
		advanceCallbackProgress(cb, t, t.Object.Size)
	}

	if a.direction == Upload {
		return api.VerifyUpload(t.Object)
	}
	return nil
}

func newCustomAdapter(name string, dir Direction, path, args string, concurrent bool) *customAdapter {
	c := &customAdapter{newAdapterBase(name, dir, nil), path, args, concurrent, 3}
	// self implements impl
	c.transferImpl = c
	return c
}

// Initialise custom adapters based on current config
func ConfigureCustomAdapters() {
	pathRegex := regexp.MustCompile(`lfs.customtransfer.([^.]+).path`)
	for k, v := range config.Config.AllGitConfig() {
		if match := pathRegex.FindStringSubmatch(k); match != nil {
			name := match[1]
			path := v
			var args string
			var concurrent bool
			var direction string
			// retrieve other values
			args, _ = config.Config.GitConfig(fmt.Sprintf("lfs.customtransfer.%s.args", name))
			concurrent = config.Config.GitConfigBool(fmt.Sprintf("lfs.customtransfer.%s.concurrent", name), true)
			direction, _ = config.Config.GitConfig(fmt.Sprintf("lfs.customtransfer.%s.direction", name))
			if len(direction) == 0 {
				direction = "both"
			} else {
				direction = strings.ToLower(direction)
			}

			// Separate closure for each since we need to capture vars above
			newfunc := func(name string, dir Direction) TransferAdapter {
				return newCustomAdapter(name, dir, path, args, concurrent)
			}

			if direction == "download" || direction == "both" {
				RegisterNewTransferAdapterFunc(name, Download, newfunc)
			}
			if direction == "upload" || direction == "both" {
				RegisterNewTransferAdapterFunc(name, Upload, newfunc)
			}

		}
	}

}

func init() {
	ConfigureCustomAdapters()
}