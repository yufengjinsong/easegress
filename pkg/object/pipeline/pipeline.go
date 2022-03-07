/*
 * Copyright (c) 2017, MegaEase
 * All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package pipeline

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/megaease/easegress/pkg/context"
	"github.com/megaease/easegress/pkg/protocols"
	"github.com/megaease/easegress/pkg/supervisor"
	"github.com/megaease/easegress/pkg/util/fasttime"
	"github.com/megaease/easegress/pkg/util/stringtool"
)

const (
	// Category is the category of Pipeline.
	Category = supervisor.CategoryPipeline

	// Kind is the kind of Pipeline.
	Kind = "Pipeline"

	// builtInFilterEnd is the name of the build-in end filter.
	builtInFilterEnd = "END"

	// defaultRequest is the name of the default request.
	defaultRequest = "Default"

	// defaultResponse is the name of the default response.
	defaultResponse = "Default"
)

func init() {
	supervisor.Register(&Pipeline{})
}

func isBuiltInFilter(name string) bool {
	return name == builtInFilterEnd
}

type (
	// Pipeline is Object Pipeline.
	Pipeline struct {
		superSpec *supervisor.Spec
		spec      *Spec

		filters map[string]Filter
		flow    []FlowNode
	}

	// Spec describes the Pipeline.
	Spec struct {
		Flow    []FlowNode               `yaml:"flow" jsonschema:"omitempty"`
		Filters []map[string]interface{} `yaml:"filters" jsonschema:"required"`
	}

	// FlowNode describes one node of the pipeline flow.
	FlowNode struct {
		Filter     string            `yaml:"filter" jsonschema:"required,format=urlname"`
		RequestID  string            `yaml:"requestID" jsonschema:"requestID,omitempty"`
		ResponseID string            `yaml:"responseID" jsonschema:"responseID,omitempty"`
		UseRequest string            `yaml:"useRequest" jsonschema:"useRequest,omitempty"`
		JumpIf     map[string]string `yaml:"jumpIf" jsonschema:"omitempty"`
		filter     Filter
	}

	// FilterStat records the statistics of a filter.
	FilterStat struct {
		Name     string
		Kind     string
		Result   string
		Duration time.Duration
	}

	// Status is the status of Pipeline.
	Status struct {
		Health  string                 `yaml:"health"`
		Filters map[string]interface{} `yaml:"filters"`
	}
)

// Validate validates Spec.
func (s *Spec) Validate() (err error) {
	errPrefix := "filters"
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s: %s", errPrefix, r)
		}
	}()

	specs := map[string]*FilterSpec{}

	// 1: validate filter spec
	for _, f := range s.Filters {
		// NOTE: Nil supervisor is fine in spec validating phrase.
		spec, err := NewFilterSpec(f, nil)
		if err != nil {
			panic(err)
		}

		name := spec.Name()
		if isBuiltInFilter(name) {
			panic(fmt.Errorf("can't use %s(built-in) for filter name", name))
		}

		specs[name] = spec
	}

	// 2: validate flow
	errPrefix = "flow"

	// 2.1: validate jumpIfs
	validNames := map[string]bool{builtInFilterEnd: true}
	for i := len(s.Flow) - 1; i >= 0; i-- {
		node := &s.Flow[i]
		if node.Filter == builtInFilterEnd {
			continue
		}
		spec := specs[node.Filter]
		if spec == nil {
			panic(fmt.Errorf("filter %s not found", node.Filter))
		}
		results := QueryFilterRegistry(spec.Kind()).Results()
		for result, target := range node.JumpIf {
			if !stringtool.StrInSlice(result, results) {
				msgFmt := "filter %s: result %s is not in %v"
				panic(fmt.Errorf(msgFmt, node.Filter, result, results))
			}
			if ok := validNames[target]; !ok {
				msgFmt := "filter %s: target filter %s not found"
				panic(fmt.Errorf(msgFmt, node.Filter, target))
			}
		}
		validNames[node.Filter] = true
	}

	// 2.2: validate request IDs
	validNames = map[string]bool{defaultRequest: true}
	for i := 0; i < len(s.Flow); i++ {
		node := &s.Flow[i]
		if node.UseRequest != "" && !validNames[node.UseRequest] {
			msgFmt := "filter %s: desired request %s not found"
			panic(fmt.Errorf(msgFmt, node.Filter, node.UseRequest))
		}
		if node.RequestID != "" {
			validNames[node.RequestID] = true
		}
	}

	return nil
}

func serializeStats(stats []FilterStat) string {
	if len(stats) == 0 {
		return "pipeline: <empty>"
	}

	var sb strings.Builder
	sb.WriteString("pipeline: ")

	for i := range stats {
		if i > 0 {
			sb.WriteString("->")
		}

		stat := &stats[i]
		sb.WriteString(stat.Name)
		sb.WriteByte('(')
		if stat.Result != "" {
			sb.WriteString(stat.Result)
			sb.WriteByte(',')
		}
		sb.WriteString(stat.Duration.String())
		sb.WriteByte(')')
	}

	return sb.String()
}

// Category returns the category of Pipeline.
func (p *Pipeline) Category() supervisor.ObjectCategory {
	return Category
}

// Kind returns the kind of Pipeline.
func (p *Pipeline) Kind() string {
	return Kind
}

// DefaultSpec returns the default spec of Pipeline.
func (p *Pipeline) DefaultSpec() interface{} {
	return &Spec{}
}

// Init initializes Pipeline.
func (p *Pipeline) Init(superSpec *supervisor.Spec, muxMapper protocols.MuxMapper) {
	p.superSpec, p.spec = superSpec, superSpec.ObjectSpec().(*Spec)

	p.reload(nil /*no previous generation*/)
}

// Inherit inherits previous generation of Pipeline.
func (p *Pipeline) Inherit(superSpec *supervisor.Spec, previousGeneration supervisor.Object, muxMapper protocols.MuxMapper) {
	p.superSpec, p.spec = superSpec, superSpec.ObjectSpec().(*Spec)

	p.reload(previousGeneration.(*Pipeline))

	// TODO: below behavior must be changed!!
	//
	// NOTE: It's filters' responsibility to inherit and clean their resources.
	// previousGeneration.Close()
}

func (p *Pipeline) reload(previousGeneration *Pipeline) {
	pipelineName := p.superSpec.Name()

	filters := make([]Filter, 0, len(p.spec.Filters))
	specs := make([]*FilterSpec, 0, len(p.spec.Filters))

	// create new filters.
	for _, rawSpec := range p.spec.Filters {
		spec, err := NewFilterSpec(rawSpec, p.superSpec.Super())
		if err != nil {
			panic(err)
		}

		rootInst := QueryFilterRegistry(spec.Kind())
		if rootInst == nil {
			panic(fmt.Errorf("kind %s not found", spec.Kind()))
		}

		spec.pipeline = pipelineName
		filter := reflect.New(reflect.TypeOf(rootInst).Elem()).Interface().(Filter)
		filters = append(filters, filter)
		specs = append(specs, spec)
	}

	// initialize or inherit the new filters.
	for i, filter := range filters {
		spec := specs[i]

		var prev Filter
		if previousGeneration != nil {
			prev = previousGeneration.filters[spec.Name()]
		}

		if prev == nil {
			filter.Init(spec)
		} else {
			filter.Inherit(spec, prev)
		}

		p.filters[spec.Name()] = filter
	}

	// if the pipeline spec does not define a flow, define it in the filter
	// appear order.
	flow := p.spec.Flow
	if len(flow) == 0 {
		flow = make([]FlowNode, 0, len(specs))
		for _, spec := range specs {
			flow = append(flow, FlowNode{Filter: spec.Name()})
		}
	}

	// bind filter instance to flow node.
	for i := range flow {
		node := &flow[i]
		if node.Filter != builtInFilterEnd {
			node.filter = p.filters[node.Filter]
		}
	}

	p.flow = flow
}

// Handle is the handler to deal with the request.
func (p *Pipeline) Handle(ctx context.HTTPContext) string {
	result, next := "", ""
	stats := make([]FilterStat, 0, len(p.flow))

	for i := range p.flow {
		node := &p.flow[i]
		if next != "" && node.Filter != next {
			continue
		}

		if node.Filter == builtInFilterEnd {
			break
		}

		start := fasttime.Now()
		if node.UseRequest == "" {
		} else {
		}

		if node.RequestID == "" {
		} else {
		}

		if node.ResponseID == "" {
		} else {
		}

		result = node.filter.Handle(ctx)
		stats = append(stats, FilterStat{
			Name:     node.Filter,
			Kind:     node.filter.Kind(),
			Duration: fasttime.Since(start),
			Result:   result,
		})

		if result != "" {
			next = node.JumpIf[result]
		}

		if next == builtInFilterEnd {
			break
		}
	}

	ctx.AddTag(serializeStats(stats))
	return result
}

// Status returns Status generated by Runtime.
func (p *Pipeline) Status() *supervisor.Status {
	s := &Status{
		Filters: make(map[string]interface{}),
	}

	for name, filter := range p.filters {
		s.Filters[name] = filter.Status()
	}

	return &supervisor.Status{
		ObjectStatus: s,
	}
}

// Close closes Pipeline.
func (p *Pipeline) Close() {
	for _, filter := range p.filters {
		filter.Close()
	}
}
