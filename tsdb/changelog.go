// Copyright 2021 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tsdb

import (
	"fmt"
	"io"

	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/tsdb/tombstones"
)

type ChangeLogger interface {
	DeleteSeries(del labels.Labels, intervals tombstones.Intervals)
	ModifySeries(old labels.Labels, new labels.Labels)
}

type changeLog struct {
	w io.Writer
}

func NewChangeLog(w io.Writer) ChangeLogger {
	return &changeLog{
		w: w,
	}
}

func (l *changeLog) DeleteSeries(del labels.Labels, intervals tombstones.Intervals) {
	_, _ = fmt.Fprintf(l.w, "Deleted %v %v\n", del.String(), intervals)
}

func (l *changeLog) ModifySeries(old, new labels.Labels) {
	_, _ = fmt.Fprintf(l.w, "Relabelled %v %v\n", old.String(), new.String())
}
