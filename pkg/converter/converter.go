// Copyright Project Harbor Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package converter

import (
	"context"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/snapshots"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/goharbor/acceleration-service/pkg/config"
	"github.com/goharbor/acceleration-service/pkg/content"
	"github.com/goharbor/acceleration-service/pkg/driver"
	"github.com/goharbor/acceleration-service/pkg/errdefs"
	"github.com/goharbor/acceleration-service/pkg/metrics"
)

var logger = logrus.WithField("module", "converter")

type Converter interface {
	// Dispatch dispatches a conversion task to worker queue
	// by specifying source image reference, the conversion is
	// asynchronous, and if the sync option is specified,
	// Dispatch will be blocked until the conversion is complete.
	Dispatch(ctx context.Context, ref string, sync bool) error
}

type LocalConverter struct {
	cfg         *config.Config
	rule        *Rule
	worker      *Worker
	client      *containerd.Client
	snapshotter snapshots.Snapshotter
	driver      driver.Driver
}

func NewLocalConverter(cfg *config.Config) (*LocalConverter, error) {
	client, err := containerd.New(
		cfg.Provider.Containerd.Address,
		containerd.WithDefaultNamespace("harbor-acceleration-service"),
	)
	if err != nil {
		return nil, errors.Wrap(err, "create containerd client")
	}
	snapshotter := client.SnapshotService(cfg.Provider.Containerd.Snapshotter)

	driver, err := driver.NewLocalDriver(&cfg.Converter.Driver)
	if err != nil {
		return nil, errors.Wrap(err, "create driver")
	}

	worker, err := NewWorker(cfg.Converter.Worker)
	if err != nil {
		return nil, errors.Wrap(err, "create worker")
	}

	rule := &Rule{
		items: cfg.Converter.Rules,
	}

	handler := &LocalConverter{
		cfg:         cfg,
		rule:        rule,
		worker:      worker,
		client:      client,
		snapshotter: snapshotter,
		driver:      driver,
	}

	return handler, nil
}

func (cvt *LocalConverter) Convert(ctx context.Context, source string) error {
	ctx, done, err := cvt.client.WithLease(ctx)
	if err != nil {
		return errors.Wrap(err, "create containerd lease")
	}
	defer done(ctx)

	target, err := cvt.rule.Map(source)
	if err != nil {
		if errors.Is(err, errdefs.ErrAlreadyConverted) {
			logrus.Infof("image has been converted: %s", source)
			return nil
		}
		return errors.Wrap(err, "create target reference by rule")
	}

	content, err := content.NewLocalProvider(
		&cvt.cfg.Provider, cvt.client, cvt.snapshotter,
	)
	if err != nil {
		return errors.Wrap(err, "create content provider")
	}

	logger.Infof("pulling image %s", source)
	if err := content.Pull(ctx, source); err != nil {
		return errors.Wrap(err, "pull image")
	}
	logger.Infof("pulled image %s", source)

	logger.Infof("converting image %s", source)
	desc, err := cvt.driver.Convert(ctx, content)
	if err != nil {
		return errors.Wrap(err, "convert image")
	}
	logger.Infof("converted image %s", target)

	logger.Infof("pushing image %s", target)
	if err := content.Push(ctx, *desc, target); err != nil {
		return errors.Wrap(err, "push image")
	}
	logger.Infof("pushed image %s", target)

	return nil
}

func (cvt *LocalConverter) Dispatch(ctx context.Context, ref string, sync bool) error {
	if sync {
		// FIXME: The synchronous conversion task should also be
		// executed in a limited worker queue.
		return cvt.Convert(context.Background(), ref)
	}

	cvt.worker.Dispatch(func() error {
		return metrics.Conversion.OpWrap(func() error {
			return cvt.Convert(context.Background(), ref)
		}, "convert")
	})

	return nil
}
