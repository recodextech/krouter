package krouter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/tryfix/kstream/admin"
	"github.com/tryfix/kstream/consumer"
	"github.com/tryfix/kstream/producer"
	"github.com/tryfix/log"
)

type someError struct {
	Code    string
	Message string
	TraceId string
}

func (s someError) Error() string {
	return "error"
}

type successResponse struct {
	Id int `json:"id"`
}

type fooRequest struct {
	Id   int    `json:"id"`
	Name string `json:"name"`
}

type fooRequestEncoder struct{}

func (t fooRequestEncoder) Encode(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

func (t fooRequestEncoder) Decode(data []byte) (interface{}, error) {
	req := fooRequest{}
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, err
	}

	return req, nil
}

var somePrehandler = func(ctx context.Context, payload HttpPayload) (interface{}, error) {
	v := payload.Body.(fooRequest)
	v.Id = 1000

	return v, nil
}

var somePrehandlerError = func(ctx context.Context, payload HttpPayload) (interface{}, error) {
	return nil, someError{
		Code:    "1000",
		Message: "some error happened",
		TraceId: "uuid",
	}
}

type somehandler struct {
	actualUserId     uuid.UUID
	actualSomeInt    int
	actualSomeString string
	actualAccid      customInt
	actualPayload    fooRequest
	mu               sync.Mutex
}

func (s *somehandler) Handle(ctx context.Context, request Payload) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actualUserId = request.Header(`user-id`).(uuid.UUID)
	s.actualSomeInt = request.Header(`some-int`).(int)
	s.actualSomeString = request.Header(`some-string`).(string)
	s.actualAccid = request.Param(`acc-id`).(customInt)
	s.actualPayload = request.Body.(fooRequest)
	return nil
}

type customInt int

func (i customInt) Val() int {
	return int(i)
}

func TestHandler_ServeHTTP_Error(t *testing.T) {
	// create a topic
	topics := admin.NewMockTopics()
	if err := topics.AddTopic(&admin.MockTopic{
		Name: "Router",
		Meta: &admin.Topic{
			Name:          "Router",
			NumPartitions: 2,
		},
	}); err != nil {
		t.Error(err)
	}

	prod := producer.NewMockProducer(topics)
	con := consumer.NewMockConsumer(topics)

	router, err := NewRouter(
		WithErrorHandlerFunc(func(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) error {
			w.Header().Add("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			e, ok := err.(someError)
			if !ok {
				return err
			}

			res := testErrorResponse{
				TraceId: e.TraceId,
				Code:    e.Code,
				Message: e.Message,
			}
			byt, err := json.Marshal(res)
			if err != nil {
				return err
			}
			_, err = w.Write(byt)
			return err
		}),
		WithConsumer(con),
		WithProducer(prod),
		WithLogger(log.Constructor.Log()),
		WithParamType(`my-custom-type`, func(v string) (i interface{}, err error) {
			d, err := strconv.Atoi(v)
			if err != nil {
				return nil, err
			}

			return customInt(d), nil
		}),
	)
	if err != nil {
		t.Error(err)
	}

	assertUserId := uuid.New()
	assertSomeString := `random-text`
	payloadhandler := new(somehandler)

	h := router.NewHandler(`route1`, fooRequestEncoder{}, somePrehandlerError,
		HandlerWithPostHandler(payloadhandler.Handle),
		HandlerWithHeader(`user-id`, ParamTypeUuid, nil),
		HandlerWithHeader(`some-int`, ParamTypeInt, nil),
		HandlerWithHeader(`some-string`, ParamTypeString, nil),
		HandlerWithHeader(`trace-id`, ParamTypeUuid, func() string { return uuid.New().String() }),
		HandlerWithParameter(`acc-id`, `my-custom-type`),
		HandlerWithKeyMapper(func(ctx context.Context, routeName string, body interface{}, params, headers map[string]string) (s string, err error) {
			return fmt.Sprint(params[`acc-id`]), nil
		}),
		HandlerWithSuccessHandlerFunc(func(ctx context.Context, w http.ResponseWriter, r *http.Request, payload HttpPayload) error {
			w.Header().Add("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			res := successResponse{Id: payload.Body.(fooRequest).Id}
			byt, err := json.Marshal(res)
			if err != nil {
				return err
			}
			_, err = w.Write(byt)
			return err
		}),
	)

	if router.kafka.enable {
		go func() {
			if err := router.Start(); err != nil {
				t.Error(err)
			}
		}()
	}

	time.Sleep(1 * time.Second)

	httpBody := fooRequest{
		Name: "some name",
	}
	testByt, err := json.Marshal(httpBody)
	if err != nil {
		t.Error(err)
	}
	buff := bytes.NewBuffer(testByt)
	req := httptest.NewRequest("POST", "http://example.com/foo/1222/bar", buff)
	req.Header.Set(`user-id`, assertUserId.String())
	req.Header.Set(`some-int`, `133`)
	req.Header.Set(`some-string`, assertSomeString)
	w := httptest.NewRecorder()

	r := mux.NewRouter()
	r.Handle(`/foo/{acc-id}/bar`, h).Methods(http.MethodPost)
	r.ServeHTTP(w, req)

	res := testErrorResponse{}
	if err := json.NewDecoder(w.Result().Body).Decode(&res); err != nil {
		t.Error(err)
	}

	if res.TraceId != `uuid` {
		t.Errorf(`expected [uuid] have [%s]`, res.TraceId)
		t.Fail()
	}

	if w.Result().StatusCode != http.StatusInternalServerError {
		t.Errorf(`expected [%d] have [%d]`, http.StatusInternalServerError, w.Result().StatusCode)
		t.Fail()
	}
}
