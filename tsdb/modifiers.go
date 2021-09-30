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
	"github.com/prometheus/prometheus/pkg/timestamp"
	"io"
	"math"
	"sort"
	"time"

	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/pkg/relabel"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/index"
	"github.com/prometheus/prometheus/tsdb/tombstones"
)

type ChangeLogger interface {
	DeleteSeries(del labels.Labels, intervals tombstones.Intervals)
	ModifySeries(old labels.Labels, new labels.Labels)
}

type changeLog struct {
	w io.Writer
}

// NewChangeLog creates a change logger writing to the given writer.
func NewChangeLog(w io.Writer) ChangeLogger {
	return &changeLog{w: w}
}

func (l *changeLog) DeleteSeries(del labels.Labels, intervals tombstones.Intervals) {
	_, _ = fmt.Fprintf(l.w, "Deleted %v %v\n", del.String(), intervals)
}

func (l *changeLog) ModifySeries(old, new labels.Labels) {
	_, _ = fmt.Fprintf(l.w, "Relabelled %v %v\n", old.String(), new.String())
}

// Modifier modifies the index symbols and chunk series before persisting a new block during compaction.
type Modifier interface {
	Modify(sym index.StringIter, set storage.ChunkSeriesSet, changeLog ChangeLogger) (index.StringIter, storage.ChunkSeriesSet, error)
}

// RelabelModifier modifies index via relabeling with changelog support.
type RelabelModifier struct {
	relabels []*relabel.Config
}

// WithRelabelModifier returns a relabel modifier.
func WithRelabelModifier(relabels ...*relabel.Config) *RelabelModifier {
	return &RelabelModifier{relabels: relabels}
}

func (d *RelabelModifier) Modify(_ index.StringIter, set storage.ChunkSeriesSet, changeLog ChangeLogger) (index.StringIter, storage.ChunkSeriesSet, error) {
	// Gather symbols.
	symbols := make(map[string]struct{})
	chunkSeriesMap := make(map[string]*mergeChunkSeries)

	for set.Next() {
		s := set.At()
		lbls := s.Labels()
		chksIter := s.Iterator()

		if processedLabels := relabel.Process(lbls, d.relabels...); len(processedLabels) == 0 {
			// Special case: Delete whole series if no labels are present.
			var (
				minT int64 = math.MaxInt64
				maxT int64 = math.MinInt64
			)
			for chksIter.Next() {
				c := chksIter.At()
				if c.MinTime < minT {
					minT = c.MinTime
				}
				if c.MaxTime > maxT {
					maxT = c.MaxTime
				}
			}

			if err := chksIter.Err(); err != nil {
				return nil, nil, err
			}

			var deleted tombstones.Intervals
			// If minTime is set then there is at least one chunk.
			if minT != math.MaxInt64 {
				deleted = deleted.Add(tombstones.Interval{Mint: minT, Maxt: maxT})
			}
			changeLog.DeleteSeries(lbls, deleted)
		} else {
			for _, lb := range processedLabels {
				symbols[lb.Name] = struct{}{}
				symbols[lb.Value] = struct{}{}
			}

			lbStr := processedLabels.String()
			if _, ok := chunkSeriesMap[lbStr]; !ok {
				chunkSeriesMap[lbStr] = newMergeChunkSeries(processedLabels)
			}
			cs := chunkSeriesMap[lbStr]

			cs.cs = append(cs.cs, &storage.ChunkSeriesEntry{
				ChunkIteratorFn: func() chunks.Iterator {
					return chksIter
				},
			})

			if !labels.Equal(lbls, processedLabels) {
				changeLog.ModifySeries(lbls, processedLabels)
			}
		}
	}

	symbolsSlice := make([]string, 0, len(symbols))
	for s := range symbols {
		symbolsSlice = append(symbolsSlice, s)
	}
	sort.Strings(symbolsSlice)

	chunkSeriesSet := make([]storage.ChunkSeries, 0, len(chunkSeriesMap))
	for _, chunkSeries := range chunkSeriesMap {
		chunkSeriesSet = append(chunkSeriesSet, chunkSeries)
	}
	sort.Slice(chunkSeriesSet, func(i, j int) bool {
		return labels.Compare(chunkSeriesSet[i].Labels(), chunkSeriesSet[j].Labels()) < 0
	})
	return index.NewStringListIter(symbolsSlice), storage.NewListChunkSeriesSet(chunkSeriesSet...), nil
}

// mergeChunkSeries merges []storage.ChunkSeries to storage.ChunkSeries.
type mergeChunkSeries struct {
	lset labels.Labels
	cs   []storage.ChunkSeries
}

func newMergeChunkSeries(lset labels.Labels) *mergeChunkSeries {
	return &mergeChunkSeries{
		lset: lset,
		cs:   make([]storage.ChunkSeries, 0),
	}
}

func (s *mergeChunkSeries) Labels() labels.Labels {
	return s.lset
}

func (s *mergeChunkSeries) Iterator() chunks.Iterator {
	if len(s.cs) == 0 {
		return nil
	}
	if len(s.cs) == 1 {
		return s.cs[0].Iterator()
	}

	return storage.NewCompactingChunkSeriesMerger(storage.ChainedSeriesMerge)(s.cs...).Iterator()
}

type retentionConf struct {
	retentionTime int64
	matchers      [][]*labels.Matcher
}

func newRetentionConfigs(now time.Time, defaultRetentionTime int64, config []*RetentionConfig) []*retentionConf {
	retentions := make([]*retentionConf, 0)
	for _, c := range config {
		if c.Retention > defaultRetentionTime {
			retentions = append(retentions, &retentionConf{
				retentionTime: timestamp.FromTime(now.Add(time.Duration(-c.Retention) * time.Millisecond)),
				matchers:      c.Matchers,
			})
		}
	}
	return retentions
}

// Go through all series and check retention matchers.
type RetentionModifier struct {
	retentions []*retentionConf
}

func WithRetentionModifier(retentions []*retentionConf) *RetentionModifier {
	return &RetentionModifier{
		retentions: retentions,
	}
}

func (d *RetentionModifier) Modify(_ index.StringIter, set storage.ChunkSeriesSet, _ ChangeLogger) (index.StringIter, storage.ChunkSeriesSet, error) {
	// Gather symbols.
	symbols := make(map[string]struct{})
	chunkSerieses := make([]storage.ChunkSeries, 0)

SeriesLoop:
	for set.Next() {
		s := set.At()
		lbls := s.Labels()
		chksIter := s.Iterator()

		for _, retention := range d.retentions {
		MatchersLoop:
			for _, matchers := range retention.matchers {
				for _, m := range matchers {
					v := lbls.Get(m.Name)

					// Only if all matchers in the deletion request are matched can we proceed to deletion.
					if v == "" || !m.Matches(v) {
						continue MatchersLoop
					}
				}

				// We found a match for the given series.
				var chk chunks.Meta
				var chks []chunks.Meta
				for chksIter.Next() {
					chk = chksIter.At()
					// Beyond the time duration.
					if chk.MaxTime < retention.retentionTime {
						continue
					}

					if chks == nil {
						chks = make([]chunks.Meta, 0)
					}
					chks = append(chks, chk)
					break
				}

				if chks != nil {
					for chksIter.Next() {
						chks = append(chks, chksIter.At())
					}
				}

				if err := chksIter.Err(); err != nil {
					return nil, nil, err
				}

				if chks != nil {
					for _, lbl := range lbls {
						symbols[lbl.Name] = struct{}{}
						symbols[lbl.Value] = struct{}{}
					}
				}

				chunkSerieses = append(chunkSerieses, &storage.ChunkSeriesEntry{
					Lset: lbls,
					ChunkIteratorFn: func() chunks.Iterator {
						return storage.NewListChunkSeriesIterator(chks...)
					},
				})
				continue SeriesLoop
			}
		}
	}

	symbolsSlice := make([]string, 0, len(symbols))
	for s := range symbols {
		symbolsSlice = append(symbolsSlice, s)
	}
	sort.Strings(symbolsSlice)

	return index.NewStringListIter(symbolsSlice), storage.NewListChunkSeriesSet(chunkSerieses...), nil
}

// Query based retention modifier. No need to go througth all series.
type RetentionQueryModifier struct {
	retentions []*retentionConf
	block      BlockReader
}

func NewRetentionQueryModifierBuilder(retentions []*retentionConf) func(block BlockReader) *RetentionQueryModifier {
	return func(block BlockReader) *RetentionQueryModifier {
		return &RetentionQueryModifier{block: block, retentions: retentions}
	}
}

func (d *RetentionQueryModifier) Modify(_ index.StringIter, _ storage.ChunkSeriesSet, _ ChangeLogger) (index.StringIter, storage.ChunkSeriesSet, error) {
	// Gather symbols.
	symbols := make(map[string]struct{})
	sets := make([]storage.ChunkSeriesSet, 0)

	for _, retention := range d.retentions {
		cq, err := NewBlockChunkQuerier(d.block, retention.retentionTime, d.block.Meta().MaxTime)
		if err != nil {
			return nil, nil, err
		}
		defer cq.Close()
		for _, matchers := range retention.matchers {
			css := cq.Select(false, nil, matchers...)
			// Iterate through series to build symbols.
			for css.Next() {
				lbls := css.At().Labels()
				for _, lbl := range lbls {
					symbols[lbl.Name] = struct{}{}
					symbols[lbl.Value] = struct{}{}
				}
			}
			if err := css.Err(); err != nil {
				return nil, nil, err
			}

			// Query again to build merged chunk seriesSet.
			css = cq.Select(false, nil, matchers...)
			sets = append(sets, css)
		}
	}

	symbolsSlice := make([]string, 0, len(symbols))
	for s := range symbols {
		symbolsSlice = append(symbolsSlice, s)
	}
	sort.Strings(symbolsSlice)
	res := storage.NewMergeChunkSeriesSet(sets, storage.NewDedupChunkSeriesMerger())
	return index.NewStringListIter(symbolsSlice), res, nil
}
