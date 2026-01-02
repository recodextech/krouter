package krouter

import "context"

type Validator interface {
	Validate(ctx context.Context, v interface{}, requestParams RequestParams) error
}
