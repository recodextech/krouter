package krouter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/tryfix/kstream/admin"
	"github.com/tryfix/kstream/consumer"
	"github.com/tryfix/kstream/producer"
	"github.com/tryfix/log"
)

type testErrorResponse struct {
	TraceId string `json:"trace_id"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func TestHandler_ServeHTTP(t *testing.T) {
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
		WithKafkaRouter(Config{
			RouterTopic: `Router`,
		}),
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
	assertSomeInt := 133
	assertSomeString := `random-text`
	assertAccid := customInt(1222)
	assertpayload := fooRequest{
		Id:   1000,
		Name: "some name",
	}

	payloadhandler := new(somehandler)
	router.kafka.enable = true
	h := router.NewHandler(`route1`, fooRequestEncoder{}, somePrehandler,
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

	go func() {
		if err := router.Start(); err != nil {
			t.Error(err)
		}
	}()

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

	res := successResponse{}
	if err := json.NewDecoder(w.Result().Body).Decode(&res); err != nil {
		t.Error(err)
	}

	if res.Id != 1000 {
		t.Fail()
	}

	if w.Result().StatusCode != http.StatusCreated {
		t.Fail()
	}

	time.Sleep(1 * time.Second)

	payloadhandler.mu.Lock()
	defer payloadhandler.mu.Unlock()

	if payloadhandler.actualUserId != assertUserId {
		t.Fail()
	}
	if payloadhandler.actualSomeInt != assertSomeInt {
		t.Fail()
	}
	if payloadhandler.actualSomeString != assertSomeString {
		t.Fail()
	}
	if payloadhandler.actualAccid != assertAccid {
		t.Fail()
	}
	if !reflect.DeepEqual(payloadhandler.actualPayload, assertpayload) {
		t.Fail()
	}
}
