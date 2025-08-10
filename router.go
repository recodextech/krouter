package krouter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Shopify/sarama"
	"github.com/google/uuid"
	"github.com/tryfix/errors"
	"github.com/tryfix/kstream/consumer"
	"github.com/tryfix/kstream/data"
	"github.com/tryfix/kstream/producer"
	"github.com/tryfix/log"
	"github.com/tryfix/metrics"
	traceable_context "github.com/tryfix/traceable-context"
)

type group struct {
	logger log.Logger
}

func (g *group) OnPartitionRevoked(ctx context.Context, revoked []consumer.TopicPartition) error {
	g.logger.InfoContext(ctx, fmt.Sprintf(`partitions revoked [%v]`, revoked))
	return nil
}

func (g *group) OnPartitionAssigned(ctx context.Context, assigned []consumer.TopicPartition) error {
	g.logger.InfoContext(ctx, fmt.Sprintf(`partitions assigned [%v]`, assigned))
	return nil
}

type Router struct {
	kafka struct {
		enable          bool
		consume         consumer.Consumer
		produce         producer.Producer
		boostrapServers []string
		routerTopic     string
		consumerGroup   string
	}
	handlers            map[string]*Handler
	logger              log.Logger
	preHandlerObserver  metrics.Observer
	postHandlerObserver metrics.Observer
	customParamTypes    map[string]CustomParam
	headersFuncs        map[string]func() string
	successHandlerFunc  SuccessHandlerFunc
	errorHandlerFunc    ErrorHandlerFunc
	contextExtractor    ContextExtractor
}

type Config struct {
	BootstrapServers []string
	RouterTopic      string
	ConsumerGroup    string
}

type routerOption func(*Router)

func WithKafkaRouter(con Config) routerOption {
	return func(r *Router) {
		r.kafka.boostrapServers = con.BootstrapServers
		r.kafka.routerTopic = con.RouterTopic
		r.kafka.consumerGroup = con.ConsumerGroup
	}
}

func WithProducer(p producer.Producer) routerOption {
	return func(r *Router) {
		r.kafka.produce = p
	}
}

func WithSuccessHandlerFunc(fn SuccessHandlerFunc) routerOption {
	return func(r *Router) {
		r.successHandlerFunc = fn
	}
}

func WithHeaderFunc(name string, fn func() string) routerOption {
	return func(r *Router) {
		r.headersFuncs[name] = fn
	}
}

func WithErrorHandlerFunc(fn ErrorHandlerFunc) routerOption {
	return func(r *Router) {
		r.errorHandlerFunc = fn
	}
}

func WithContextExtractor(fn ContextExtractor) routerOption {
	return func(r *Router) {
		r.contextExtractor = fn
	}
}

func WithConsumer(c consumer.Consumer) routerOption {
	return func(r *Router) {
		r.kafka.consume = c
	}
}

func WithLogger(l log.Logger) routerOption {
	return func(r *Router) {
		r.logger = l
	}
}

func WithMetricsReporter(reporter metrics.Reporter) routerOption {
	return func(r *Router) {
		r.preHandlerObserver = reporter.Observer(metrics.MetricConf{
			Path:        "pre_request_latency",
			Labels:      []string{`type`, `error`},
			ConstLabels: nil,
		})

		r.postHandlerObserver = reporter.Observer(metrics.MetricConf{
			Path:        "post_request_latency",
			Labels:      []string{`type`, `error`},
			ConstLabels: nil,
		})
	}
}

func WithParamType(name string, decoder func(v string) (interface{}, error)) routerOption {
	return func(r *Router) {
		r.customParamTypes[name] = CustomParam{
			typ:     ParamType(name),
			decoder: decoder,
		}
	}
}

func NewRouter(options ...routerOption) (*Router, error) {
	r := &Router{
		headersFuncs:     map[string]func() string{},
		handlers:         map[string]*Handler{},
		logger:           log.NewNoopLogger(),
		customParamTypes: map[string]CustomParam{},
		contextExtractor: func(r *http.Request) context.Context {
			return r.Context()
		},
	}

	empty := struct {
		Path        string
		Labels      []string
		ConstLabels map[string]string
	}{Path: "", Labels: nil, ConstLabels: nil}
	r.preHandlerObserver = metrics.NoopReporter().Observer(empty)
	r.postHandlerObserver = metrics.NoopReporter().Observer(empty)

	for _, opt := range options {
		opt(r)
	}

	if len(r.kafka.boostrapServers) > 0 && r.kafka.consumerGroup != "" {
		cConfig := consumer.NewConsumerConfig()
		cConfig.BootstrapServers = r.kafka.boostrapServers
		cConfig.GroupId = r.kafka.consumerGroup
		cConfig.Version = sarama.V2_4_0_0
		c, err := consumer.NewConsumer(cConfig, consumer.WithRecordUuidExtractFunc(func(message *data.Record) uuid.UUID {
			traceId := message.Headers.Read([]byte(`trace_id`))
			uid, err := uuid.Parse(string(traceId))
			if err != nil {
				r.logger.Error(`trace-id does not exist creating new id`)
				return uuid.New()
			}

			return uid
		}))
		if err != nil {
			return nil, errors.WithPrevious(err, `Router init failed`)
		}
		r.kafka.consume = c
		r.kafka.enable = true
	}

	if len(r.kafka.boostrapServers) > 0 {
		pConfig := producer.NewConfig()
		pConfig.BootstrapServers = r.kafka.boostrapServers
		pConfig.Version = sarama.V2_4_0_0
		pConfig.RequiredAcks = producer.WaitForAll
		p, err := producer.NewProducer(pConfig)
		if err != nil {
			return nil, errors.WithPrevious(err, `Router init failed`)
		}
		r.kafka.produce = p
	}

	return r, nil
}

func (r *Router) NewHandler(name string, encoder Encoder, preHandler PreRouteHandleFunc, options ...handlerOption) http.Handler {
	h := &Handler{
		preHandler:       preHandler,
		encode:           encoder,
		name:             name,
		router:           r,
		logger:           r.logger,
		errorHandlerFunc: r.errorHandlerFunc,
		headersFuncs:     r.headersFuncs,
		contextExtractor: r.contextExtractor,
	}

	for _, opt := range options {
		opt(h)
	}

	_, ok := r.handlers[name]
	if ok {
		panic(fmt.Sprintf(`postHandler [%s] already registered`, name))
	}

	r.handlers[name] = h
	return h
}

func (r *Router) Start() error {
	// start consumer
	partitions, err := r.kafka.consume.Consume([]string{r.kafka.routerTopic}, &group{logger: r.logger})
	if err != nil {
		return errors.WithPrevious(err, `Router consumer start failed`)
	}

	for p := range partitions {
		go r.startPartition(p)
	}

	return nil
}

func (r *Router) startPartition(p consumer.Partition) {
	for record := range p.Records() {
		ctx := traceable_context.WithUUID(record.UUID)
		if err := r.process(ctx, record); err != nil {
			r.logger.ErrorContext(ctx, record.UUID, err)
		}
	}
}

func (r *Router) process(ctx context.Context, record *data.Record) error {
	var err error
	route := Route{}
	if err = json.Unmarshal(record.Value, &route); err != nil {
		return errors.WithPrevious(err, fmt.Sprintf(`re-route roiute decode error on route [%s]`, route.Name))
	}

	// post handler metrics
	defer func(start time.Time) {
		elapsed := time.Now().Sub(start).Milliseconds()
		r.postHandlerObserver.Observe(func(e int64) float64 {
			return float64(e)
		}(elapsed), map[string]string{"type": route.Name, "error": fmt.Sprintf("%v", err != nil)})
	}(time.Now())

	h, ok := r.handlers[route.Name]
	if !ok {
		return errors.New(fmt.Sprintf(`postHandler [%s] not registered`, route.Name))
	}

	params, err := h.decodeParams(h.supportedParams, route.Params)
	if err != nil {
		return errors.WithPrevious(err, fmt.Sprintf(`parameter decode error on route [%s]`, route.Name))
	}

	headers, err := h.decodeParams(h.supportedHeaders, route.Headers)
	if err != nil {
		return errors.WithPrevious(err, fmt.Sprintf(`header decode error on route [%s]`, route.Name))
	}

	payload := Payload{
		params:  params,
		headers: headers,
		Body:    nil,
	}

	v, err := h.encode.Decode([]byte(route.Payload))
	if err != nil {
		return errors.WithPrevious(err, fmt.Sprintf(`re-route payload decode error on route [%s]`, route.Name))
	}

	payload.Body = v

	if err := h.postHandler(ctx, payload); err != nil {
		return errors.WithPrevious(err, fmt.Sprintf(`postHandler error on route [%s]`, route.Name))
	}

	return nil
}
