package krouter

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/Shopify/sarama"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/tryfix/errors"
	"github.com/tryfix/kstream/data"
	"github.com/tryfix/log"
	traceable_context "github.com/tryfix/traceable-context"
)

type InvalidHeaderError struct {
	Name string
}

type ContextExtractor func(r *http.Request) context.Context

func (i InvalidHeaderError) Error() string {
	return fmt.Sprintf(`invalid header value or header does not exist for http header [%s]`, i.Name)
}

type RequestParams interface {
	Param(name string) string
	Header(name string) string
}

type Payload struct {
	headers map[string]interface{}
	params  map[string]interface{}
	Body    interface{}
}

type HttpPayload struct {
	headers map[string]string
	params  map[string]string
	Body    interface{}
}

func (p *HttpPayload) Param(name string) string {
	return p.params[name]
}

func (p *HttpPayload) Header(name string) string {
	return p.headers[name]
}

type PreRouteHandleFunc func(ctx context.Context, payload HttpPayload) (interface{}, error)

type PostRouteHandleFunc func(ctx context.Context, payload Payload) error

func (p *Payload) Param(name string) interface{} {
	return p.params[name]
}

func (p *Payload) Header(name string) interface{} {
	return p.headers[name]
}

type handlerOption func(*Handler)

func HandlerWithPostHandler(postHandler PostRouteHandleFunc) handlerOption {
	return func(h *Handler) {
		h.postHandler = postHandler
	}
}

func HandlerWithValidator(v Validator) handlerOption {
	return func(h *Handler) {
		h.validators = append(h.validators, v)
	}
}

func HandlerWithKeyMapper(km KeyMapper) handlerOption {
	return func(h *Handler) {
		h.keyMapper = km
	}
}

func HandlerWithHeader(name string, typ ParamType, whenEmpty func() string) handlerOption {
	return func(h *Handler) {
		h.supportedHeaders = append(h.supportedHeaders, Param{
			name:      name,
			typ:       typ,
			whenEmpty: whenEmpty,
		})
	}
}

func HandlerWithParameter(name string, typ ParamType) handlerOption {
	return func(h *Handler) {
		h.supportedParams = append(h.supportedParams, Param{
			name: name,
			typ:  typ,
		})
	}
}

func HandlerWithSuccessHandlerFunc(fn SuccessHandlerFunc) handlerOption {
	return func(h *Handler) {
		h.successHandlerFunc = fn
	}
}

func HandlerWithErrorHandlerFunc(fn ErrorHandlerFunc) handlerOption {
	return func(h *Handler) {
		h.errorHandlerFunc = fn
	}
}

func HandlerWithHeaderFunc(name string, fn func() string) handlerOption {
	return func(h *Handler) {
		h.headersFuncs[name] = fn
	}
}

func HandlerWithContextExtractor(fn ContextExtractor) handlerOption {
	return func(h *Handler) {
		h.contextExtractor = fn
	}
}

type KeyMapper func(ctx context.Context, routeName string, body interface{}, params, headers map[string]string) (string, error)

type Handler struct {
	request            *Payload
	logger             log.Logger
	postHandler        PostRouteHandleFunc
	preHandler         PreRouteHandleFunc
	validators         []Validator
	encode             Encoder
	supportedHeaders   []Param
	supportedParams    []Param
	router             *Router
	name               string
	headersFuncs       map[string]func() string
	keyMapper          KeyMapper
	successHandlerFunc SuccessHandlerFunc
	errorHandlerFunc   ErrorHandlerFunc
	contextExtractor   ContextExtractor
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// create a context
	ctx := h.contextExtractor(r)
	if err := h.serve(ctx, w, r); err != nil {
		h.logger.ErrorContext(ctx, fmt.Sprintf(`http serve error preHandler error due to %s`, err))
		if err := h.errorHandlerFunc(ctx, w, r, err); err != nil {
			h.logger.ErrorContext(ctx, fmt.Sprintf(`http request error preHandler error due to %s`, err))
		}
	}
}

func (h *Handler) serve(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	rawParams := map[string]string{}
	rawHeaders := map[string]string{}

	// apply http request headers to re-route headers
	for _, h := range h.supportedHeaders {
		if r.Header.Get(h.name) != `` {
			rawHeaders[h.name] = r.Header.Get(h.name)
			continue
		}
		if h.whenEmpty == nil {
			return InvalidHeaderError{
				Name: h.name,
			}
		}
		rawHeaders[h.name] = h.whenEmpty()
	}

	// apply http Router params to re-route params
	vars := mux.Vars(r)
	for _, h := range h.supportedParams {
		v, ok := vars[h.name]
		if !ok {
			return fmt.Errorf(`route param [%s] does not exist in request`, h.name)
		}
		rawParams[h.name] = v
	}

	// read request
	byt, err := io.ReadAll(r.Body)
	if err != nil {
		return errors.WithPrevious(err, fmt.Sprintf(`http request read failed on route [%s]`, h.name))
	}

	// pre handler metrics
	defer func(start time.Time) {
		elapsed := time.Now().Sub(start).Milliseconds()
		h.router.preHandlerObserver.Observe(func(e int64) float64 {
			return float64(e)
		}(elapsed), map[string]string{"type": h.name, "error": fmt.Sprintf("%v", err != nil)})
	}(time.Now())

	// decode http request through provided encoder
	v, err := h.encode.Decode(byt)
	if err != nil {
		return errors.WithPrevious(err, fmt.Sprintf(`http request decode error on payload [%s]`, string(byt)))
	}

	payload := HttpPayload{
		headers: rawHeaders,
		params:  rawParams,
		Body:    v,
	}

	// apply validators
	for _, validator := range h.validators {
		if err := validator.Validate(ctx, v, &payload); err != nil {
			return err
		}
	}

	// call pre route preHandler
	processed, err := h.preHandler(ctx, payload)
	if err != nil {
		return err
	}

	payload.Body = processed

	byt, err = h.encode.Encode(processed)
	if err != nil {
		return errors.WithPrevious(err, fmt.Sprintf(`pre payload request failed [%s]`, string(byt)))
	}

	h.logger.InfoContext(ctx, payload, string(byt))

	// prepare re-route payload
	route := Route{
		Params:  rawParams,
		Headers: rawHeaders,
		Payload: string(byt),
		Name:    h.name,
	}

	// encode the re-route payload
	req, err := route.Encode()
	if err != nil {
		return errors.WithPrevious(err, fmt.Sprintf(`re-route request encode failed on payload [%v]`, payload))
	}

	// default key mapper will be the postHandler name
	key := h.name
	if h.keyMapper != nil {
		// get the route key
		key, err = h.keyMapper(ctx, h.name, v, rawParams, rawHeaders)
		if err != nil {
			return errors.WithPrevious(err, fmt.Sprintf(`re-route key extract error on payload [%v]`, payload))
		}
	}

	traceId := traceable_context.FromContext(ctx)
	if traceId == uuid.Nil {
		h.logger.WarnContext(ctx, `empty uuid in the context setting a new one`)
		traceId = uuid.New()
	}
	if h.router.kafka.enable {
		// produce to re-route topic
		p, o, err := h.router.kafka.produce.Produce(ctx, &data.Record{
			Key:       []byte(key),
			Value:     req,
			Topic:     h.router.kafka.routerTopic,
			Timestamp: time.Now(),
			Headers: data.RecordHeaders{&sarama.RecordHeader{
				Key:   []byte(`trace_id`),
				Value: []byte(traceId.String()),
			}},
		})
		if err != nil {
			return errors.WithPrevious(err, fmt.Sprintf(`re-route message send error on payload [%v]`, payload))
		}
		h.logger.TraceContext(ctx, fmt.Sprintf(`re-route message [%s] sent to %s[%d] with key [%s] at offset %d`, string(req), h.router.kafka.routerTopic, p, key, o))
	}

	if err := h.successHandlerFunc(ctx, w, r, payload); err != nil {
		h.logger.Error(fmt.Sprintf(`http request success postHandler error due to %s`, err))
	}

	return nil
}

func (h *Handler) decodeParams(supported []Param, params map[string]string) (map[string]interface{}, error) {
	decoded := make(map[string]interface{})
	for _, p := range supported {
		v, ok := params[p.name]
		if !ok {
			return nil, fmt.Errorf(`route parameter %s does not exist on route [%s]`, p.name, h.name)
		}

		switch p.typ {
		case ParamTypeString:
			decoded[p.name] = v
		case ParamTypeUuid:
			i, err := uuid.Parse(v)
			if err != nil {
				return nil, errors.WithPrevious(err, fmt.Sprintf(`route parameter [%s] decode error on route [%s]`, p.name, h.name))
			}

			decoded[p.name] = i
		case ParamTypeInt:
			i, err := strconv.Atoi(v)
			if err != nil {
				return nil, errors.WithPrevious(err, fmt.Sprintf(`route parameter [%s] decode error on route [%s]`, p.name, h.name))
			}

			decoded[p.name] = i
		default:
			// check if paramType exist in custom paramTypes
			typ, ok := h.router.customParamTypes[string(p.typ)]
			if !ok {
				return nil, fmt.Errorf(`route parameter %s[%s] is not a valid param type on route [%s]`, p.name, reflect.TypeOf(params[p.name]), h.name)
			}

			i, err := typ.decoder(v)
			if err != nil {
				return nil, errors.WithPrevious(err, fmt.Sprintf(`route parameter [%s] decode error for custom type [%s] on route [%s]`, p.name, typ.typ, h.name))
			}
			decoded[p.name] = i
		}
	}

	return decoded, nil
}
