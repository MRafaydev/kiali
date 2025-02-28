package business

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/kiali/kiali/jaeger"
	jaegerModels "github.com/kiali/kiali/jaeger/model/json"
	"github.com/kiali/kiali/log"
	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/observability"
)

type JaegerLoader = func() (jaeger.ClientInterface, error)
type SpanFilter = func(span *jaegerModels.Span) bool

type JaegerService struct {
	loader        JaegerLoader
	loaderErr     error
	jaeger        jaeger.ClientInterface
	businessLayer *Layer
}

func (in *JaegerService) client() (jaeger.ClientInterface, error) {
	if in.jaeger != nil {
		return in.jaeger, nil
	} else if in.loaderErr != nil {
		return nil, in.loaderErr
	}
	in.jaeger, in.loaderErr = in.loader()
	return in.jaeger, in.loaderErr
}

func (in *JaegerService) getFilteredSpans(ns, app string, query models.TracingQuery, filter SpanFilter) ([]jaeger.JaegerSpan, error) {
	r, err := in.GetAppTraces(ns, app, query)
	if err != nil {
		return []jaeger.JaegerSpan{}, err
	}
	spans := tracesToSpans(app, r, filter)
	return spans, nil
}

func mergeResponses(dest *jaeger.JaegerResponse, src *jaeger.JaegerResponse) {
	dest.JaegerServiceName = src.JaegerServiceName
	dest.Errors = append(dest.Errors, src.Errors...)
	traceIds := make(map[jaegerModels.TraceID]bool)
	for _, prev := range dest.Data {
		traceIds[prev.TraceID] = true
	}
	for _, trace := range src.Data {
		if _, ok := traceIds[trace.TraceID]; !ok {
			dest.Data = append(dest.Data, trace)
			traceIds[trace.TraceID] = true
		}
	}
}

func (in *JaegerService) GetAppSpans(ns, app string, query models.TracingQuery) ([]jaeger.JaegerSpan, error) {
	return in.getFilteredSpans(ns, app, query, nil /*no post-filtering for apps*/)
}

func (in *JaegerService) GetServiceSpans(ctx context.Context, ns, service string, query models.TracingQuery) ([]jaeger.JaegerSpan, error) {
	var end observability.EndFunc
	ctx, end = observability.StartSpan(ctx, "GetServiceSpans",
		observability.Attribute("package", "business"),
		observability.Attribute("namespace", ns),
		observability.Attribute("service", service),
	)
	defer end()

	app, err := in.businessLayer.Svc.GetServiceAppName(ctx, ns, service)
	if err != nil {
		return nil, err
	}
	var postFilter SpanFilter
	// Run post-filter only for service != app
	if app != service {
		postFilter = operationSpanFilter(ns, service)
	}
	return in.getFilteredSpans(ns, app, query, postFilter)
}

func operationSpanFilter(ns, service string) SpanFilter {
	fqService := service + "." + ns
	// Filter out app spans based on operation name.
	// For envoy traces, operation name is like "service-name.namespace.svc.cluster.local:8000/*"
	return func(span *jaegerModels.Span) bool {
		return strings.HasPrefix(span.OperationName, fqService)
	}
}

func (in *JaegerService) GetWorkloadSpans(ctx context.Context, ns, workload string, query models.TracingQuery) ([]jaeger.JaegerSpan, error) {
	var end observability.EndFunc
	ctx, end = observability.StartSpan(ctx, "GetWorkloadSpans",
		observability.Attribute("package", "business"),
		observability.Attribute("namespace", ns),
		observability.Attribute("workload", workload),
	)
	defer end()

	app, err := in.businessLayer.Workload.GetWorkloadAppName(ctx, ns, workload)
	if err != nil {
		return nil, err
	}
	return in.getFilteredSpans(ns, app, query, wkdSpanFilter(ns, workload))
}

func wkdSpanFilter(ns, workload string) SpanFilter {
	// Filter out app traces based on the node_id tag, that contains workload information.
	return func(span *jaegerModels.Span) bool {
		return spanMatchesWorkload(span, ns, workload)
	}
}

func (in *JaegerService) GetAppTraces(ns, app string, query models.TracingQuery) (*jaeger.JaegerResponse, error) {
	client, err := in.client()
	if err != nil {
		return nil, err
	}
	r, err := client.GetAppTraces(ns, app, query)
	if err != nil {
		return nil, err
	}
	if len(r.Data) == query.Limit {
		// Reached the limit, use split & join mode to spread traces over the requested interval
		log.Trace("Limit of traces was reached, using split & join mode")
		more, err := in.getAppTracesSlicedInterval(ns, app, query)
		if err != nil {
			// Log error but continue to process results (might still have some data fetched)
			log.Errorf("Traces split & join failed: %v", err)
		}
		if more != nil {
			mergeResponses(r, more)
		}
	}
	return r, nil
}

// GetServiceTraces returns traces involving the requested service.  Note that because the tracing API pulls traces by "App", only a
// subset of the traces may actually involve the requested service.  Callers may need to upwardly adjust TracingQuery.Limit to get back
// the number of desired traces.  It depends on the number of services backing the app. For example, if there are 2 services for the
// app, if evenly distributed, a query limit of 20 may return only 10 traces.  The ratio is typically not as bad as it is with
// GetWorkloadTraces.
func (in *JaegerService) GetServiceTraces(ctx context.Context, ns, service string, query models.TracingQuery) (*jaeger.JaegerResponse, error) {
	var end observability.EndFunc
	ctx, end = observability.StartSpan(ctx, "GetServiceTraces",
		observability.Attribute("package", "business"),
		observability.Attribute("namespace", ns),
		observability.Attribute("service", service),
	)
	defer end()

	app, err := in.businessLayer.Svc.GetServiceAppName(ctx, ns, service)
	if err != nil {
		return nil, err
	}
	if app == service {
		// No post-filtering
		return in.GetAppTraces(ns, app, query)
	}

	r, err := in.GetAppTraces(ns, app, query)
	if r != nil && err == nil {
		// Filter out app traces based on operation name.
		// For envoy traces, operation name is like "service-name.namespace.svc.cluster.local:8000/*"
		filter := operationSpanFilter(ns, service)
		traces := []jaegerModels.Trace{}
		for _, trace := range r.Data {
			for _, span := range trace.Spans {
				if filter(&span) {
					traces = append(traces, trace)
					break
				}
			}
		}
		r.Data = traces
	}
	return r, err
}

// GetWorkloadTraces returns traces involving the requested workload.  Note that because the tracing API pulls traces by "App", only
// a subset of the traces may actually involve the requested workload.  Callers may need to upwardly adjust TracingQuery.Limit to get back
// the number of desired traces.  It depends on the number of workloads backing the app. For example, if there are 5 workloads for the
// app, if evenly distributed, a query limit of 25 may return only 5 traces.
func (in *JaegerService) GetWorkloadTraces(ctx context.Context, ns, workload string, query models.TracingQuery) (*jaeger.JaegerResponse, error) {
	var end observability.EndFunc
	ctx, end = observability.StartSpan(ctx, "GetWorkloadTraces",
		observability.Attribute("package", "business"),
		observability.Attribute("namespace", ns),
		observability.Attribute("workload", workload),
	)
	defer end()

	app, err := in.businessLayer.Workload.GetWorkloadAppName(ctx, ns, workload)
	if err != nil {
		return nil, err
	}

	r, err := in.GetAppTraces(ns, app, query)
	// Filter out app traces based on the node_id tag, that contains workload information.
	if r != nil && err == nil {
		traces := []jaegerModels.Trace{}
		for _, trace := range r.Data {
			if matchesWorkload(&trace, ns, workload) {
				traces = append(traces, trace)
			}
		}
		r.Data = traces
	}
	return r, err
}

func (in *JaegerService) getAppTracesSlicedInterval(ns, app string, query models.TracingQuery) (*jaeger.JaegerResponse, error) {
	client, err := in.client()
	if err != nil {
		return nil, err
	}
	// Spread queries over 10 interval slices
	nSlices := 10
	limit := query.Limit / nSlices
	if limit == 0 {
		limit = 1
	}
	diff := query.End.Sub(query.Start)
	duration := diff / time.Duration(nSlices)

	type tracesChanResult struct {
		resp *jaeger.JaegerResponse
		err  error
	}
	tracesChan := make(chan tracesChanResult, nSlices)
	var wg sync.WaitGroup

	for i := 0; i < nSlices; i++ {
		q := query
		q.Limit = limit
		q.Start = query.Start.Add(duration * time.Duration(i))
		q.End = q.Start.Add(duration)
		wg.Add(1)
		go func(q models.TracingQuery) {
			defer wg.Done()
			r, err := client.GetAppTraces(ns, app, q)
			tracesChan <- tracesChanResult{resp: r, err: err}
		}(q)
	}
	wg.Wait()
	// All slices are fetched, close channel
	close(tracesChan)
	merged := &jaeger.JaegerResponse{}
	for r := range tracesChan {
		if r.err != nil {
			err = r.err
			continue
		}
		mergeResponses(merged, r.resp)
	}
	return merged, err
}

func (in *JaegerService) GetJaegerTraceDetail(traceID string) (trace *jaeger.JaegerSingleTrace, err error) {
	client, err := in.client()
	if err != nil {
		return nil, err
	}
	return client.GetTraceDetail(traceID)
}

func (in *JaegerService) GetErrorTraces(ns, app string, duration time.Duration) (errorTraces int, err error) {
	client, err := in.client()
	if err != nil {
		return 0, err
	}
	return client.GetErrorTraces(ns, app, duration)
}

func (in *JaegerService) GetStatus() (accessible bool, err error) {
	client, err := in.client()
	if err != nil {
		return false, err
	}
	return client.GetServiceStatus()
}

func matchesWorkload(trace *jaegerModels.Trace, namespace, workload string) bool {
	for _, span := range trace.Spans {
		if process, ok := trace.Processes[span.ProcessID]; ok {
			span.Process = &process
		}
		if spanMatchesWorkload(&span, namespace, workload) {
			return true
		}
	}
	return false
}

func spanMatchesWorkload(span *jaegerModels.Span, namespace, workload string) bool {
	// For envoy traces, with a workload named "ai-locals", node_id is like:
	// sidecar~172.17.0.20~ai-locals-6d8996bff-ztg6z.default~default.svc.cluster.local
	for _, tag := range span.Tags {
		if tag.Key == "node_id" {
			if v, ok := tag.Value.(string); ok {
				parts := strings.Split(v, "~")
				if len(parts) >= 3 && strings.HasPrefix(parts[2], workload) && strings.HasSuffix(parts[2], namespace) {
					return true
				}
			}
		}
	}
	// Tag not found => try with 'hostname' in process' tags
	if span.Process != nil {
		for _, tag := range span.Process.Tags {
			if tag.Key == "hostname" {
				if v, ok := tag.Value.(string); ok {
					if strings.HasPrefix(v, workload) {
						return true
					}
				}
			}
		}
	}
	return false
}

func tracesToSpans(app string, r *jaeger.JaegerResponse, filter SpanFilter) []jaeger.JaegerSpan {
	spans := []jaeger.JaegerSpan{}
	for _, trace := range r.Data {
		// First, get the desired processes for our service
		processes := make(map[jaegerModels.ProcessID]jaegerModels.Process)
		for pId, process := range trace.Processes {
			if process.ServiceName == app || process.ServiceName == r.JaegerServiceName {
				processes[pId] = process
			}
		}
		// Second, find spans for these processes
		for _, span := range trace.Spans {
			if p, ok := processes[span.ProcessID]; ok {
				span.Process = &p
				if filter == nil || filter(&span) {
					spans = append(spans, jaeger.JaegerSpan{
						Span:      span,
						TraceSize: len(trace.Spans),
					})
				}
			}
		}
	}
	log.Tracef("Found %d spans in the %d traces for app %s", len(spans), len(r.Data), app)
	return spans
}
