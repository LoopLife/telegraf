//go:generate ../../../tools/readme_config_includer/generator
package influxdb_v2_listener

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/internal"
	tlsint "github.com/influxdata/telegraf/plugins/common/tls"
	"github.com/influxdata/telegraf/plugins/inputs"
	"github.com/influxdata/telegraf/plugins/parsers/influx"
	"github.com/influxdata/telegraf/plugins/parsers/influx/influx_upstream"
	"github.com/influxdata/telegraf/selfstat"
)

//go:embed sample.conf
var sampleConfig string

const (
	// defaultMaxBodySize is the default maximum request body size, in bytes.
	// if the request body is over this size, we will return an HTTP 413 error.
	defaultMaxBodySize  = 32 * 1024 * 1024
	defaultReadTimeout  = 10 * time.Second
	defaultWriteTimeout = 10 * time.Second
)

var ErrEOF = errors.New("EOF")

// The BadRequestCode constants keep standard error messages
// see: https://v2.docs.influxdata.com/v2.0/api/#operation/PostWrite
type BadRequestCode string

const (
	InternalError BadRequestCode = "internal error"
	Invalid       BadRequestCode = "invalid"
)

type InfluxDBV2Listener struct {
	ServiceAddress string `toml:"service_address"`
	port           int
	tlsint.ServerConfig

	ReadTimeout  config.Duration `toml:"read_timeout"`
	WriteTimeout config.Duration `toml:"write_timeout"`
	MaxBodySize  config.Size     `toml:"max_body_size"`
	Token        string          `toml:"token"`
	BucketTag    string          `toml:"bucket_tag"`
	ParserType   string          `toml:"parser_type"`

	timeFunc influx.TimeFunc

	listener net.Listener
	server   http.Server

	acc telegraf.Accumulator

	bytesRecv       selfstat.Stat
	requestsServed  selfstat.Stat
	writesServed    selfstat.Stat
	readysServed    selfstat.Stat
	requestsRecv    selfstat.Stat
	notFoundsServed selfstat.Stat
	authFailures    selfstat.Stat

	startTime time.Time

	Log telegraf.Logger `toml:"-"`

	mux http.ServeMux
}

func (*InfluxDBV2Listener) SampleConfig() string {
	return sampleConfig
}

func (h *InfluxDBV2Listener) Gather(_ telegraf.Accumulator) error {
	return nil
}

func (h *InfluxDBV2Listener) routes() {
	credentials := ""
	if h.Token != "" {
		credentials = fmt.Sprintf("Token %s", h.Token)
	}
	authHandler := internal.GenericAuthHandler(credentials,
		func(_ http.ResponseWriter) {
			h.authFailures.Incr(1)
		},
	)

	h.mux.Handle("/api/v2/write", authHandler(h.handleWrite()))
	h.mux.Handle("/api/v2/ready", h.handleReady())
	h.mux.Handle("/", authHandler(h.handleDefault()))
}

func (h *InfluxDBV2Listener) Init() error {
	tags := map[string]string{
		"address": h.ServiceAddress,
	}
	h.bytesRecv = selfstat.Register("influxdb_v2_listener", "bytes_received", tags)
	h.requestsServed = selfstat.Register("influxdb_v2_listener", "requests_served", tags)
	h.writesServed = selfstat.Register("influxdb_v2_listener", "writes_served", tags)
	h.readysServed = selfstat.Register("influxdb_v2_listener", "readys_served", tags)
	h.requestsRecv = selfstat.Register("influxdb_v2_listener", "requests_received", tags)
	h.notFoundsServed = selfstat.Register("influxdb_v2_listener", "not_founds_served", tags)
	h.authFailures = selfstat.Register("influxdb_v2_listener", "auth_failures", tags)
	h.routes()

	if h.MaxBodySize == 0 {
		h.MaxBodySize = config.Size(defaultMaxBodySize)
	}

	if h.ReadTimeout < config.Duration(time.Second) {
		h.ReadTimeout = config.Duration(defaultReadTimeout)
	}
	if h.WriteTimeout < config.Duration(time.Second) {
		h.WriteTimeout = config.Duration(defaultWriteTimeout)
	}

	return nil
}

// Start starts the InfluxDB listener service.
func (h *InfluxDBV2Listener) Start(acc telegraf.Accumulator) error {
	h.acc = acc

	tlsConf, err := h.ServerConfig.TLSConfig()
	if err != nil {
		return err
	}

	h.server = http.Server{
		Addr:         h.ServiceAddress,
		Handler:      h,
		TLSConfig:    tlsConf,
		ReadTimeout:  time.Duration(h.ReadTimeout),
		WriteTimeout: time.Duration(h.WriteTimeout),
	}

	var listener net.Listener
	if tlsConf != nil {
		listener, err = tls.Listen("tcp", h.ServiceAddress, tlsConf)
		if err != nil {
			return err
		}
	} else {
		listener, err = net.Listen("tcp", h.ServiceAddress)
		if err != nil {
			return err
		}
	}
	h.listener = listener
	h.port = listener.Addr().(*net.TCPAddr).Port

	go func() {
		err = h.server.Serve(h.listener)
		if !errors.Is(err, http.ErrServerClosed) {
			h.Log.Infof("Error serving HTTP on %s", h.ServiceAddress)
		}
	}()

	h.startTime = h.timeFunc()

	h.Log.Infof("Started HTTP listener service on %s", h.ServiceAddress)

	return nil
}

// Stop cleans up all resources
func (h *InfluxDBV2Listener) Stop() {
	err := h.server.Shutdown(context.Background())
	if err != nil {
		h.Log.Infof("Error shutting down HTTP server: %v", err.Error())
	}
}

func (h *InfluxDBV2Listener) ServeHTTP(res http.ResponseWriter, req *http.Request) {
	h.requestsRecv.Incr(1)
	h.mux.ServeHTTP(res, req)
	h.requestsServed.Incr(1)
}

func (h *InfluxDBV2Listener) handleReady() http.HandlerFunc {
	return func(res http.ResponseWriter, req *http.Request) {
		defer h.readysServed.Incr(1)

		// respond to ready requests
		res.Header().Set("Content-Type", "application/json")
		res.WriteHeader(http.StatusOK)
		b, _ := json.Marshal(map[string]string{
			"started": h.startTime.Format(time.RFC3339Nano),
			"status":  "ready",
			"up":      h.timeFunc().Sub(h.startTime).String()})
		if _, err := res.Write(b); err != nil {
			h.Log.Debugf("error writing in handle-ready: %v", err)
		}
	}
}

func (h *InfluxDBV2Listener) handleDefault() http.HandlerFunc {
	return func(res http.ResponseWriter, req *http.Request) {
		defer h.notFoundsServed.Incr(1)
		http.NotFound(res, req)
	}
}

func (h *InfluxDBV2Listener) handleWrite() http.HandlerFunc {
	return func(res http.ResponseWriter, req *http.Request) {
		defer h.writesServed.Incr(1)
		// Check that the content length is not too large for us to handle.
		if req.ContentLength > int64(h.MaxBodySize) {
			if err := tooLarge(res, int64(h.MaxBodySize)); err != nil {
				h.Log.Debugf("error in too-large: %v", err)
			}
			return
		}

		bucket := req.URL.Query().Get("bucket")

		body := req.Body
		body = http.MaxBytesReader(res, body, int64(h.MaxBodySize))
		// Handle gzip request bodies
		if req.Header.Get("Content-Encoding") == "gzip" {
			var err error
			body, err = gzip.NewReader(body)
			if err != nil {
				h.Log.Debugf("Error decompressing request body: %v", err.Error())
				if err := badRequest(res, Invalid, err.Error()); err != nil {
					h.Log.Debugf("error in bad-request: %v", err)
				}
				return
			}
			defer body.Close()
		}

		var readErr error
		var bytes []byte
		//body = http.MaxBytesReader(res, req.Body, 1000000) //p.MaxBodySize.Size)
		bytes, readErr = io.ReadAll(body)
		if readErr != nil {
			h.Log.Debugf("Error parsing the request body: %v", readErr.Error())
			if err := badRequest(res, InternalError, readErr.Error()); err != nil {
				h.Log.Debugf("error in bad-request: %v", err)
			}
			return
		}

		precisionStr := req.URL.Query().Get("precision")

		var metrics []telegraf.Metric
		var err error
		if h.ParserType == "upstream" {
			parser := influx_upstream.Parser{}
			err = parser.Init()
			if !errors.Is(err, ErrEOF) && err != nil {
				h.Log.Debugf("Error initializing parser: %v", err.Error())
				return
			}
			parser.SetTimeFunc(influx_upstream.TimeFunc(h.timeFunc))

			if precisionStr != "" {
				precision := getPrecisionMultiplier(precisionStr)
				if err = parser.SetTimePrecision(precision); err != nil {
					h.Log.Debugf("Error setting precision of parser: %v", err)
					return
				}
			}

			metrics, err = parser.Parse(bytes)
		} else {
			parser := influx.Parser{}
			err = parser.Init()
			if !errors.Is(err, ErrEOF) && err != nil {
				h.Log.Debugf("Error initializing parser: %v", err.Error())
				return
			}
			parser.SetTimeFunc(h.timeFunc)

			if precisionStr != "" {
				precision := getPrecisionMultiplier(precisionStr)
				parser.SetTimePrecision(precision)
			}

			metrics, err = parser.Parse(bytes)
		}

		if !errors.Is(err, ErrEOF) && err != nil {
			h.Log.Debugf("Error parsing the request body: %v", err.Error())
			if err := badRequest(res, Invalid, err.Error()); err != nil {
				h.Log.Debugf("error in bad-request: %v", err)
			}
			return
		}

		for _, m := range metrics {
			// Handle bucket_tag override
			if h.BucketTag != "" && bucket != "" {
				m.AddTag(h.BucketTag, bucket)
			}

			h.acc.AddMetric(m)
		}

		// http request success
		res.WriteHeader(http.StatusNoContent)
	}
}

func tooLarge(res http.ResponseWriter, maxLength int64) error {
	res.Header().Set("Content-Type", "application/json")
	res.Header().Set("X-Influxdb-Error", "http: request body too large")
	res.WriteHeader(http.StatusRequestEntityTooLarge)
	b, _ := json.Marshal(map[string]string{
		"code":      fmt.Sprint(Invalid),
		"message":   "http: request body too large",
		"maxLength": fmt.Sprint(maxLength)})
	_, err := res.Write(b)
	return err
}

func badRequest(res http.ResponseWriter, code BadRequestCode, errString string) error {
	res.Header().Set("Content-Type", "application/json")
	if errString == "" {
		errString = "http: bad request"
	}
	res.Header().Set("X-Influxdb-Error", errString)
	res.WriteHeader(http.StatusBadRequest)
	b, _ := json.Marshal(map[string]string{
		"code":    fmt.Sprint(code),
		"message": errString,
		"op":      "",
		"err":     errString,
	})
	_, err := res.Write(b)
	return err
}

func getPrecisionMultiplier(precision string) time.Duration {
	// Influxdb defaults silently to nanoseconds if precision isn't
	// one of the following:
	var d time.Duration
	switch precision {
	case "us":
		d = time.Microsecond
	case "ms":
		d = time.Millisecond
	case "s":
		d = time.Second
	default:
		d = time.Nanosecond
	}
	return d
}

func init() {
	inputs.Add("influxdb_v2_listener", func() telegraf.Input {
		return &InfluxDBV2Listener{
			ServiceAddress: ":8086",
			timeFunc:       time.Now,
		}
	})
}
