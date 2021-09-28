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
	"math"
	"sort"

	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/pkg/relabel"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/index"
	"github.com/prometheus/prometheus/tsdb/tombstones"
)

type Modifier interface {
	Modify(sym index.StringIter, set storage.ChunkSeriesSet) (index.StringIter, storage.ChunkSeriesSet)
}

type RelabelModifier struct {
	relabels []*relabel.Config
	log      ChangeLogger
}

func WithRelabelModifier(log ChangeLogger, relabels ...*relabel.Config) *RelabelModifier {
	return &RelabelModifier{relabels: relabels, log: log}
}

func (d *RelabelModifier) Modify(_ index.StringIter, set storage.ChunkSeriesSet) (index.StringIter, storage.ChunkSeriesSet) {
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
				return errorOnlyStringIter{err: err}, nil
			}

			var deleted tombstones.Intervals
			// If minTime is set then there is at least one chunk.
			if minT != math.MaxInt64 {
				deleted = deleted.Add(tombstones.Interval{Mint: minT, Maxt: maxT})
			}
			d.log.DeleteSeries(lbls, deleted)
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
				Lset: nil,
				ChunkIteratorFn: func() chunks.Iterator {
					return chksIter
				},
			})

			if !labels.Equal(lbls, processedLabels) {
				d.log.ModifySeries(lbls, processedLabels)
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
	return index.NewStringListIter(symbolsSlice), storage.NewListChunkSeriesSet(chunkSeriesSet...)
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

type errorOnlyStringIter struct {
	err error
}

func (errorOnlyStringIter) Next() bool   { return false }
func (errorOnlyStringIter) At() string   { return "" }
func (s errorOnlyStringIter) Err() error { return s.err }
