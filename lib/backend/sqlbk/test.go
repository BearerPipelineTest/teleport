/*
Copyright 2018-2022 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sqlbk

import (
	"context"
	"testing"

	"github.com/gravitational/teleport/lib/backend"
	"github.com/gravitational/teleport/lib/backend/test"
	"github.com/gravitational/trace"

	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"
)

// TestDriver executes the backend compliance suite for a driver. A single
// backend is created so connections remain open for all subtests.
func TestDriver(t *testing.T, driver Driver) {
	bk, err := New(context.Background(), driver)
	require.NoError(t, err)
	t.Cleanup(func() { bk.Close() })

	fakeClock, ok := driver.Config().Clock.(clockwork.FakeClock)
	require.True(t, ok, "expected %v driver to configure a FakeClock", driver.BackendName())

	newBackend := func(options ...test.ConstructionOption) (backend.Backend, clockwork.FakeClock, error) {
		opts, err := test.ApplyOptions(options)
		if err != nil {
			return nil, nil, trace.Wrap(err)
		}

		if opts.MirrorMode {
			return nil, nil, test.ErrMirrorNotSupported
		}

		bk := &testBackend{Backend: bk}
		bk.buf = backend.NewCircularBuffer(backend.BufferCapacity(bk.BufferSize))
		bk.buf.SetInit()
		return bk, fakeClock, nil
	}
	test.RunBackendComplianceSuite(t, newBackend)
}

// testBackend wraps Backend overriding Close.
type testBackend struct {
	*Backend
}

// Close only the buffer so buffer watchers are notified of close events.
func (b *testBackend) Close() error {
	return b.buf.Close()
}
