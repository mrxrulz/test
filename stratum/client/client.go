// Copyright 2020 cryptonote.social. All rights reserved. Use of this source code is governed by
// the license found in the LICENSE file.

// package client implements a basic stratum client that listens to jobs and
// can submit shares
package client

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cryptonote-social/csminer/crylog"
	"io"
	"net"
	"time"
)

const (
	SUBMIT_WORK_JSON_ID = 999
	CONNECT_JSON_ID     = 666

	MAX_REQUEST_SIZE = 10000 // Max # of bytes we will read per request

	NO_WALLET_SPECIFIED_WARNING_CODE = 2
)

type Job struct {
	Blob   string `json:"blob"`
	JobID  string `json:"job_id"`
	Target string `json:"target"`
	Algo   string `json:"algo"`
	// For self-select mode:
	PoolWallet string `json:"pool_wallet"`
	ExtraNonce string `json:"extra_nonce"`
}

type RXJob struct {
	Job
	Height   int    `json:"height"`
	SeedHash string `json:"seed_hash"`
}

type ForknoteJob struct {
	Job
	MajorVersion int `json:"blockMajorVersion"`
	MinorVersion int `json:"blockMinerVersion"`
}

type MultiClientJob struct {
	RXJob
	NetworkDifficulty int64  `json:"net_diff"`
	Reward            int64  `json:"reward"`
	ConnNonce         uint32 `json:"nonce"`
}

type Client struct {
	JobChannel chan *MultiClientJob
	FirstJob   *MultiClientJob

	address         string
	agent           string
	conn            net.Conn
	responseChannel chan *SubmitWorkResponse
}

func NewClient(address string, agent string) *Client {
	return &Client{
		address: address,
		agent:   agent,
	}
}

// Connect to the stratum server port with the given login info. Returns error if connection could
// not be established, or if the stratum server itself returned an error. In the latter case,
// code and message will also be specified. If the stratum server returned just a warning, then
// error will be nil, but code & message will be specified.
func (cl *Client) Connect(uname, pw, rigid string, useTLS bool) (err error, code int, message string) {
	if !useTLS {
		cl.conn, err = net.DialTimeout("tcp", cl.address, time.Second*30)
	} else {
		cl.conn, err = tls.Dial("tcp", cl.address, nil /*Config*/)
	}
	if err != nil {
		crylog.Error("Dial failed:", err, cl)
		return err, 0, ""
	}
	cl.responseChannel = make(chan *SubmitWorkResponse)
	cl.JobChannel = make(chan *MultiClientJob)
	// send login
	loginRequest := &struct {
		ID     uint64      `json:"id"`
		Method string      `json:"method"`
		Params interface{} `json:"params"`
	}{
		ID:     CONNECT_JSON_ID,
		Method: "login",
		Params: &struct {
			Login string `json:"login"`
			Pass  string `json:"pass"`
			RigID string `json:"rigid"`
			Agent string `json:"agent"`
		}{
			Login: uname,
			Pass:  pw,
			RigID: rigid,
			Agent: cl.agent,
		},
	}

	data, err := json.Marshal(loginRequest)
	if err != nil {
		crylog.Error("json marshalling failed:", err, "for client:", cl)
		return err, 0, ""
	}
	cl.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	data = append(data, '\n')
	if _, err = cl.conn.Write(data); err != nil {
		crylog.Error("writing request failed:", err, "for client:", cl)
		return err, 0, ""
	}

	// Now read the login response
	response := &struct {
		ID      uint64 `json:"id"`
		Jsonrpc string `json:"jsonrpc"`
		Result  *struct {
			ID  string          `json:"id"`
			Job *MultiClientJob `job:"job"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		// our own custom field for reporting login warnings without forcing disconnect from error:
		Warning *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"warning"`
	}{}
	cl.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	rdr := bufio.NewReaderSize(cl.conn, MAX_REQUEST_SIZE)
	//d, _, _ := rdr.ReadLine()
	//crylog.Info("login response:", string(d))
	err = readJSON(response, rdr)
	if err != nil {
		crylog.Error("readJSON failed for client:", cl, err)
		return err, 0, ""
	}
	if response.Result == nil {
		crylog.Error("Didn't get job result from login response:", response.Error)
		return errors.New("stratum server error"), response.Error.Code, response.Error.Message
	}
	cl.FirstJob = response.Result.Job
	if response.Warning != nil {
		return nil, response.Warning.Code, response.Warning.Message
	}
	return nil, 0, ""
}

func (cl *Client) SubmitMulticlientWork(username string, rigid string, nonce string, connNonce []byte, jobid string, targetDifficulty int64) (*SubmitWorkResponse, error) {
	submitRequest := &struct {
		ID     uint64      `json:"id"`
		Method string      `json:"method"`
		Params interface{} `json:"params"`
	}{
		ID:     SUBMIT_WORK_JSON_ID,
		Method: "submit",
		Params: &struct {
			ID     string `json:"id"`
			JobID  string `json:"job_id"`
			Nonce  string `json:"nonce"`
			Result string `json:"result"`
			// Fields below are used by profit-maximizing servicea
			ForUser       string `json:"for_user"`
			ForRig        string `json:"for_rig"`
			ForDifficulty int64  `json:"for_difficulty"`
			ConnNonce     []byte `json:"conn_nonce"`
		}{"696969", jobid, nonce, "", username, rigid, targetDifficulty, connNonce},
	}

	return cl.submitRequest(submitRequest)
}

func (cl *Client) submitRequest(submitRequest interface{}) (*SubmitWorkResponse, error) {
	data, err := json.Marshal(submitRequest)
	if err != nil {
		crylog.Error("json marshalling failed:", err, "for client:", cl)
		return nil, err
	}
	cl.conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
	data = append(data, '\n')
	if _, err = cl.conn.Write(data); err != nil {
		crylog.Error("writing request failed:", err, "for client:", cl)
		return nil, err
	}
	timeout := make(chan bool)
	go func() {
		time.Sleep(30 * time.Second)
		timeout <- true
	}()
	var response *SubmitWorkResponse
	select {
	case response = <-cl.responseChannel:
	case <-timeout:
		crylog.Error("response timeout")
		return nil, fmt.Errorf("submit work failure: response timeout")
	}
	if response == nil {
		crylog.Error("got nil response")
		return nil, fmt.Errorf("submit work failure: nil response")
	}
	if response.ID != SUBMIT_WORK_JSON_ID {
		crylog.Error("got unexpected response:", response.ID)
		return nil, fmt.Errorf("submit work failure: unexpected response")
	}
	return response, nil
}

func (cl *Client) SubmitWork(nonce string, jobid string) (*SubmitWorkResponse, error) {
	submitRequest := &struct {
		ID     uint64      `json:"id"`
		Method string      `json:"method"`
		Params interface{} `json:"params"`
	}{
		ID:     SUBMIT_WORK_JSON_ID,
		Method: "submit",
		Params: &struct {
			ID     string `json:"id"`
			JobID  string `json:"job_id"`
			Nonce  string `json:"nonce"`
			Result string `json:"result"`
		}{"696969", jobid, nonce, ""},
	}
	return cl.submitRequest(submitRequest)
}

func (cl *Client) String() string {
	return "stratum_client:" + cl.address
}

func (cl *Client) Close() {
	cl.conn.Close()
	close(cl.JobChannel)
	close(cl.responseChannel)
}

type SubmitWorkResponse struct {
	ID      uint64                 `json:"id"`
	Jsonrpc string                 `json:"jsonrpc"`
	Method  string                 `json:"method"`
	Job     *MultiClientJob        `json:"params"`
	Result  map[string]interface{} `json:"result"`
	Error   map[string]interface{} `json:"error"`
}

// DispatchJobs will forward incoming jobs to the JobChannel until error is received or the
// connection is closed.
func (cl *Client) DispatchJobs() error {
	cl.JobChannel <- cl.FirstJob
	cl.FirstJob = nil
	reader := bufio.NewReaderSize(cl.conn, MAX_REQUEST_SIZE)
	for {
		response := &SubmitWorkResponse{}
		cl.conn.SetReadDeadline(time.Now().Add(3600 * time.Second))
		err := readJSON(response, reader)
		if err != nil {
			crylog.Error("readJSON failed:", err)
			return err
		}
		if response.Method != "job" {
			if response.ID == SUBMIT_WORK_JSON_ID {
				cl.responseChannel <- response
				continue
			}
			crylog.Warn("Unexpected response:", *response)
			continue
		}
		if response.Job == nil {
			crylog.Error("Didn't get job:", *response)
			return errors.New("didn't get job as expected")
		}
		cl.JobChannel <- response.Job
	}
}

func readJSON(response interface{}, reader *bufio.Reader) error {
	data, isPrefix, err := reader.ReadLine()
	if isPrefix {
		crylog.Warn("oversize request")
		return errors.New("oversize request")
	} else if err == io.EOF {
		crylog.Info("eof")
		return err
	} else if err != nil {
		crylog.Warn("error reading:", err)
		return err
	}
	err = json.Unmarshal(data, response)
	if err != nil {
		crylog.Warn("failed to unmarshal json stratum login response:", err)
		return err
	}
	return nil
}
