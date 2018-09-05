package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Masterminds/sprig"
	"github.com/davecgh/go-spew/spew"
	"github.com/jessevdk/go-flags"
	"github.com/pkg/errors"
	"github.com/reddec/fluent-amqp"
	"github.com/streadway/amqp"
	"io"
	"io/ioutil"
	"log"
	"os"
	"text/template"
	"time"
)

var (
	version = "dev"
)

var config struct {
	URLs         []string `short:"u" long:"url"                env:"BROKER_URL"         description:"One or more AMQP brokers urls" default:"amqp://guest:guest@localhost" required:"yes"`
	Exchange     string   `short:"e" long:"exchange"           env:"BROKER_EXCHANGE"    description:"Name of AMQP exchange. Can be empty"`
	ExchangeType string   `short:"k" long:"kind"               env:"BROKER_KIND"        description:"Exchange kind" choice:"direct" choice:"topic" choice:"fanout" default:"direct"`
	Verify       string   `short:"s" long:"verify-public-cert" env:"BROKER_SIGN"        description:"Path to public cert to verify"`
	Queue        string   `short:"Q" long:"queue"              env:"BROKER_QUEUE"       description:"Queue name or empty for autogenerated"`
	Lazy         bool     `short:"l" long:"lazy"               env:"BROKER_LAZY"        description:"Make queue lazy (prefer keep data on disk)"`
	OutType      string   `short:"o" long:"output"             env:"BROKER_OUTPUT"      description:"Output type" choice:"body" choice:"dump" choice:"json" choice:"template" default:"body"`
	Args         struct {
		RoutingKey string `positional-arg-name:"routing-key" env:"BROKER_ROUTING_KEY" description:"Routing key"`
	} `positional-args:"yes"`

	Interval time.Duration `short:"R" long:"reconnect-interval" env:"BROKER_RECONNECT_INTERVAL" description:"Reconnect timeout" default:"5s"`
	Timeout  time.Duration `short:"T" long:"timeout" env:"BROKER_CONNECT_TIMEOUT" description:"Connect timeout" default:"30s"`
	Quiet    bool          `short:"q" long:"quiet" env:"BROKER_QUIET" description:"Suppress all log messages"`
	Version  bool          `short:"v" long:"version" description:"Print version and exit"`
}

var logOutput io.Writer = os.Stderr

type dumpHandler struct{}

func (dh *dumpHandler) Handle(ctx context.Context, msg amqp.Delivery) {
	spew.Dump(msg)
}

type jsonHandler struct{}

func (dh *jsonHandler) Handle(ctx context.Context, msg amqp.Delivery) {
	dec := json.NewEncoder(os.Stdout)
	dec.SetIndent("", "  ")
	err := dec.Encode(&msg)
	if err != nil {
		panic(err)
	}
}

type plainHandler struct{}

func (dh *plainHandler) Handle(ctx context.Context, msg amqp.Delivery) {
	_, err := os.Stdout.Write(msg.Body)
	if err != nil {
		panic(err)
	}
}

type templateHandler struct {
	t *template.Template
}

func (dh *templateHandler) Handle(ctx context.Context, msg amqp.Delivery) {
	err := dh.t.Execute(os.Stdout, msg)
	if err != nil {
		panic(err)
	}
}

func run() error {
	gctx, cancel := context.WithCancel(context.Background())
	ctx := fluent.SignalContext(gctx)
	broker := fluent.Broker(config.URLs...).Context(ctx).Logger(log.New(logOutput, "[broker] ", log.LstdFlags)).Interval(config.Interval).Timeout(config.Timeout).Start()
	defer broker.WaitToFinish()
	defer cancel()
	log.Println("preparing sink")
	publisherCfg := broker.Sink(config.Queue)
	if config.Verify != "" {
		log.Println("preparing validator")
		publisherCfg = publisherCfg.Validate(config.Verify)
	}
	if config.Lazy {
		publisherCfg = publisherCfg.Lazy()
	}

	var handler fluent.SimpleHandler
	switch config.OutType {
	case "dump":
		handler = &dumpHandler{}
	case "json":
		handler = &jsonHandler{}
	case "body", "plain":
		handler = &plainHandler{}
	case "template":
		log.Println("reading template from STDIN")
		data, err := ioutil.ReadAll(os.Stdin)
		if err != nil && err != io.EOF {
			return err
		}
		funcs := sprig.TxtFuncMap()
		funcs["asText"] = func(data []byte) string { return string(data) }
		t, err := template.New("").Funcs(funcs).Parse(string(data))
		if err != nil {
			return err
		}
		handler = &templateHandler{t}
	default:
		panic("unknown output format")
	}
	handlerFunc := func(ctx context.Context, msg amqp.Delivery) {
		handler.Handle(ctx, msg)
		cancel()
	}
	var exc *fluent.Exchange
	if config.Exchange != "" {
		switch config.ExchangeType {
		case "topic":
			exc = publisherCfg.Topic(config.Exchange)
		case "direct":
			exc = publisherCfg.Direct(config.Exchange)
		case "fanout":
			exc = publisherCfg.Fanout(config.Exchange)
		default:
			return errors.Errorf("unknown exchange type %v", config.ExchangeType)
		}

		if config.Args.RoutingKey != "" {
			exc = exc.Key(config.Args.RoutingKey)
		}

		exc.HandlerFunc(handlerFunc)
	} else {
		publisherCfg.HandlerFunc(handlerFunc)
	}
	log.Println("reader prepared")
	log.Println("waiting for messages...")
	broker.WaitToFinish()
	return nil
}

func main() {
	parser := flags.NewParser(&config, flags.Default)
	_, err := parser.Parse()
	if config.Version {
		fmt.Println(version)
		return
	}
	if err != nil {
		os.Exit(1)
	}
	if config.Quiet {
		logOutput = ioutil.Discard
	}
	log.SetPrefix("[recv  ] ")
	log.SetOutput(logOutput)
	err = run()
	if err != nil {
		log.Println("failed:", err)
		os.Exit(2)
	}
}
