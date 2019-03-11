package syslog

import (
	"crypto/tls"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/influxdata/go-syslog/nontransparent"
	"github.com/influxdata/go-syslog/rfc5424"
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/internal"
	tlsint "github.com/influxdata/telegraf/internal/tls"
	"github.com/influxdata/telegraf/plugins/outputs"
)

type Syslog struct {
	Address         string
	KeepAlivePeriod *internal.Duration
	DefaultSdid     string
	DefaultPriority uint64
	DefaultAppname  string
	Sdids           []string
	Framing         Framing
	Trailer         nontransparent.TrailerType
	Separator       string `toml:"sdparam_separator"`
	net.Conn
	tlsint.ClientConfig
	reservedFields map[string]bool
}

var sampleConfig = `
## URL to connect to
# address = "tcp://127.0.0.1:8094"
# address = "tcp://example.com:http"
# address = "tcp4://127.0.0.1:8094"
# address = "tcp6://127.0.0.1:8094"
# address = "tcp6://[2001:db8::1]:8094"
# address = "udp://127.0.0.1:8094"
# address = "udp4://127.0.0.1:8094"
# address = "udp6://127.0.0.1:8094"

## Optional TLS Config
# tls_ca = "/etc/telegraf/ca.pem"
# tls_cert = "/etc/telegraf/cert.pem"
# tls_key = "/etc/telegraf/key.pem"
## Use TLS but skip chain & host verification
# insecure_skip_verify = false

## Period between keep alive probes.
## Only applies to TCP sockets.
## 0 disables keep alive probes.
## Defaults to the OS configuration.
# keep_alive_period = "5m"

## The framing technique with which it is expected that messages are transported (default = "octet-counting").
## Whether the messages come using the octect-counting (RFC5425#section-4.3.1, RFC6587#section-3.4.1),
## or the non-transparent framing technique (RFC6587#section-3.4.2).
## Must be one of "octect-counting", "non-transparent".
# framing = "octet-counting"

## The trailer to be expected in case of non-trasparent framing (default = "LF").
## Must be one of "LF", or "NUL".
# trailer = "LF"

### SD-PARAMs settings
### A syslog message can contain multiple parameters and multiple identifiers within structured data section
### For each unrecognised metric field a SD-PARAMS can be created. 
### Example
### Configuration =>
### sdparam_separator = "_"
### default_sdid = "default@32473"
### sdids = ["foo@123", "bar@456"]
### input => xyzzy,x=y foo@123_value=42,bar@456_value2=84,something_else=1
### output (structured data only) => [foo@123 value=42][bar@456 value2=84][default@32473 something_else=1 x=y]

## SD-PARAMs separator between the sdid and field key (default = "_") 
sdparam_separator = "_"

## Default sdid used for for fields that don't contain a prefix defined in the explict sdids setting below
## If no default is specified, no SD-PARAMs will be used for unrecognised field.
#default_sdid = "default@32473"

##List of explicit prefixes to extract from fields and use as the SDID, if they match (see above example for more details):
#sdids = ["foo@123", "bar@456"]
###

# Default PRI value (RFC5424#section-6.2.1) If no metric Field with key "PRI" is defined, this default value is used.
default_priority = 0

# Default APP-NAME value (RFC5424#section-6.2.5) If no metric Field with key "APP-NAME" is defined, this default value is used.
default_appname = "Telegraf"
`

func (s *Syslog) Connect() error {
	spl := strings.SplitN(s.Address, "://", 2)
	if len(spl) != 2 {
		return fmt.Errorf("invalid address: %s", s.Address)
	}

	tlsCfg, err := s.ClientConfig.TLSConfig()
	if err != nil {
		return err
	}

	var c net.Conn
	if tlsCfg == nil {
		c, err = net.Dial(spl[0], spl[1])
	} else {
		c, err = tls.Dial(spl[0], spl[1], tlsCfg)
	}
	if err != nil {
		return err
	}

	if err := s.setKeepAlive(c); err != nil {
		log.Printf("unable to configure keep alive (%s): %s", s.Address, err)
	}

	s.Conn = c
	return nil
}

func (s *Syslog) setKeepAlive(c net.Conn) error {
	if s.KeepAlivePeriod == nil {
		return nil
	}
	tcpc, ok := c.(*net.TCPConn)
	if !ok {
		return fmt.Errorf("cannot set keep alive on a %s socket", strings.SplitN(s.Address, "://", 2)[0])
	}
	if s.KeepAlivePeriod.Duration == 0 {
		return tcpc.SetKeepAlive(false)
	}
	if err := tcpc.SetKeepAlive(true); err != nil {
		return err
	}
	return tcpc.SetKeepAlivePeriod(s.KeepAlivePeriod.Duration)
}

func (s *Syslog) Close() error {
	if s.Conn == nil {
		return nil
	}
	err := s.Conn.Close()
	s.Conn = nil
	return err
}

func (s *Syslog) SampleConfig() string {
	return sampleConfig
}

func (s *Syslog) Description() string {
	return "Configuration for Syslog server to send metrics to"
}

func (s *Syslog) Write(metrics []telegraf.Metric) error {
	if s.Conn == nil {
		// previous write failed with permanent error and socket was closed.
		if err := s.Connect(); err != nil {
			return err
		}
	}

	for _, metric := range metrics {
		if msg, err := s.mapMetricToSyslogMessage(metric); err == nil {
			msgBytesWithObjectCounting := s.getSyslogMessageBytesWithFraming(msg)
			if _, err := s.Conn.Write(msgBytesWithObjectCounting); err != nil {
				if err, ok := err.(net.Error); !ok || !err.Temporary() {
					s.Close()
					s.Conn = nil
					return fmt.Errorf("closing connection: %v", err)
				}
				return err
			}
		}
	}
	return nil
}

func formatValue(value interface{}) string {
	switch v := value.(type) {
	case string:
		return value.(string)
	case bool:
		if v {
			return "1"
		}
		return "0"
	case uint64:
		return strconv.FormatUint(v, 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		if math.IsNaN(v) {
			return ""
		}

		if math.IsInf(v, 0) {
			return ""
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	}

	return ""
}

func (s *Syslog) mapMetricToSyslogMessage(metric telegraf.Metric) (*rfc5424.SyslogMessage, error) {
	msg := &rfc5424.SyslogMessage{}
	msg.SetVersion(1)
	msg.SetTimestamp(metric.Time().Format(time.RFC3339))

	if value, ok := metric.GetField("PRI"); ok {
		if v, err := strconv.ParseUint(formatValue(value), 10, 8); err == nil {
			msg.SetPriority(uint8(v))
		}
	} else {
		//Use default PRI
		msg.SetPriority(uint8(s.DefaultPriority))
	}
	if value, ok := metric.GetField("APP-NAME"); ok {
		msg.SetAppname(formatValue(value))
	} else {
		//Use default APP-NAME
		msg.SetAppname(s.DefaultAppname)
	}
	if value, ok := metric.GetField("MSGID"); ok {
		msg.SetMsgID(formatValue(value))
	} else {
		// We default to metric name
		msg.SetMsgID(metric.Name())
	}
	// Try with HOSTNAME, then with SOURCE, then take OS Hostname
	if value, ok := metric.GetField("HOSTNAME"); ok {
		msg.SetHostname(formatValue(value))
	} else if value, ok := metric.GetField("SOURCE"); ok {
		msg.SetHostname(formatValue(value))
	} else if value, err := os.Hostname(); err == nil {
		msg.SetHostname(value)
	}
	if value, ok := metric.GetField("PROCID"); ok {
		msg.SetProcID(formatValue(value))
	}
	if value, ok := metric.GetField("MSG"); ok {
		msg.SetMessage(formatValue(value))
	}

	for key, value := range metric.Fields() {
		if s.reservedFields[key] {
			continue
		}
		isExplicitSdid := false
		for _, sdid := range s.Sdids {
			k := strings.TrimLeft(key, sdid+s.Separator)
			if len(key) > len(k) {
				isExplicitSdid = true
				msg.SetParameter(sdid, k, formatValue(value))
				break
			}
		}
		if !isExplicitSdid && len(s.DefaultSdid) > 0 {
			k := strings.TrimLeft(key, s.DefaultSdid+s.Separator)
			msg.SetParameter(s.DefaultSdid, k, formatValue(value))
		}
	}
	if !msg.Valid() {
		return msg, fmt.Errorf("Not enough information in the metric to create a valid Syslog message")
	}
	return msg, nil
}

func (s *Syslog) getSyslogMessageBytesWithFraming(msg *rfc5424.SyslogMessage) []byte {
	msgString, _ := msg.String()
	msgBytes := []byte(msgString)

	if s.Framing == OctetCounting {
		return append([]byte(strconv.Itoa(len(msgBytes))+" "), msgBytes...)
	}
	// Non-transparent framing
	return append(msgBytes, byte(s.Trailer))
}

func newSyslog() *Syslog {
	return &Syslog{
		reservedFields: map[string]bool{
			"PRI": true, "HOSTNAME": true, "APP-NAME": true,
			"PROCID": true, "MSGID": true, "MSG": true},
		Framing:         OctetCounting,
		Trailer:         nontransparent.LF,
		Separator:       "_",
		DefaultPriority: uint64(0),
		DefaultAppname:  "Telegraf",
	}
}

func init() {
	outputs.Add("syslog", func() telegraf.Output { return newSyslog() })
}