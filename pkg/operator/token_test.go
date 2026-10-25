package operator

import (
	"context"
	"errors"
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

type fakeServerLister struct {
	err         error
	sawDeadline bool
	perPage     int
}

func (f *fakeServerLister) List(ctx context.Context, opts hcloud.ServerListOpts) ([]*hcloud.Server, *hcloud.Response, error) {
	_, f.sawDeadline = ctx.Deadline()
	f.perPage = opts.PerPage
	return nil, nil, f.err
}

func TestValidateToken(t *testing.T) {
	for name, tc := range map[string]struct {
		err         error
		wantErr     bool
		wantWarning bool
	}{
		"valid token":         {err: nil},
		"unauthorized":        {err: hcloud.Error{Code: hcloud.ErrorCodeUnauthorized}, wantErr: true},
		"forbidden":           {err: hcloud.Error{Code: hcloud.ErrorCodeForbidden}, wantErr: true},
		"api outage":          {err: hcloud.Error{Code: hcloud.ErrorCodeServiceError}, wantWarning: true},
		"rate limited":        {err: hcloud.Error{Code: hcloud.ErrorCodeRateLimitExceeded}, wantWarning: true},
		"network unreachable": {err: errors.New("dial tcp: i/o timeout"), wantWarning: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeServerLister{err: tc.err}
			warning, err := ValidateToken(context.Background(), f)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want error: %v", err, tc.wantErr)
			}
			if (warning != nil) != tc.wantWarning {
				t.Errorf("warning = %v, want warning: %v", warning, tc.wantWarning)
			}
			if !f.sawDeadline {
				t.Error("token check ran without a deadline")
			}
			if f.perPage != 1 {
				t.Errorf("token check listed %d per page, want the cheapest read (1)", f.perPage)
			}
		})
	}
}
