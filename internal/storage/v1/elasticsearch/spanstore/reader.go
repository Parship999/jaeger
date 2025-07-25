// Copyright (c) 2019 The Jaeger Authors.
// Copyright (c) 2017 Uber Technologies, Inc.
// SPDX-License-Identifier: Apache-2.0

package spanstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/olivere/elastic/v7"
	"go.opentelemetry.io/collector/featuregate"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/jaegertracing/jaeger-idl/model/v1"
	es "github.com/jaegertracing/jaeger/internal/storage/elasticsearch"
	cfg "github.com/jaegertracing/jaeger/internal/storage/elasticsearch/config"
	"github.com/jaegertracing/jaeger/internal/storage/elasticsearch/dbmodel"
)

const (
	spanIndexBaseName    = "jaeger-span-"
	serviceIndexBaseName = "jaeger-service-"
	traceIDAggregation   = "traceIDs"
	indexPrefixSeparator = "-"

	traceIDField           = "traceID"
	durationField          = "duration"
	startTimeField         = "startTime"
	startTimeMillisField   = "startTimeMillis"
	serviceNameField       = "process.serviceName"
	operationNameField     = "operationName"
	objectTagsField        = "tag"
	objectProcessTagsField = "process.tag"
	nestedTagsField        = "tags"
	nestedProcessTagsField = "process.tags"
	nestedLogFieldsField   = "logs.fields"
	tagKeyField            = "key"
	tagValueField          = "value"

	defaultNumTraces = 100

	dawnOfTimeSpanAge = time.Hour * 24 * 365 * 50
)

var (
	// ErrServiceNameNotSet occurs when attempting to query with an empty service name
	ErrServiceNameNotSet = errors.New("service Name must be set")

	// ErrStartTimeMinGreaterThanMax occurs when start time min is above start time max
	ErrStartTimeMinGreaterThanMax = errors.New("start Time Minimum is above Maximum")

	// ErrDurationMinGreaterThanMax occurs when duration min is above duration max
	ErrDurationMinGreaterThanMax = errors.New("duration Minimum is above Maximum")

	// ErrMalformedRequestObject occurs when a request object is nil
	ErrMalformedRequestObject = errors.New("malformed request object")

	// ErrStartAndEndTimeNotSet occurs when start time and end time are not set
	ErrStartAndEndTimeNotSet = errors.New("start and End Time must be set")

	// ErrUnableToFindTraceIDAggregation occurs when an aggregation query for TraceIDs fail.
	ErrUnableToFindTraceIDAggregation = errors.New("could not find aggregation of traceIDs")

	defaultMaxDuration = model.DurationAsMicroseconds(time.Hour * 24)

	objectTagFieldList = []string{objectTagsField, objectProcessTagsField}

	nestedTagFieldList = []string{nestedTagsField, nestedProcessTagsField, nestedLogFieldsField}

	_ CoreSpanReader = (*SpanReader)(nil) // check API conformance

	disableLegacyIDs *featuregate.Gate
)

func init() {
	disableLegacyIDs = featuregate.GlobalRegistry().MustRegister(
		"jaeger.es.disableLegacyId",
		featuregate.StageStable, // enabled by default and cannot be disabled
		featuregate.WithRegisterFromVersion("v2.5.0"),
		featuregate.WithRegisterToVersion("v2.8.0"),
		featuregate.WithRegisterDescription("Legacy trace ids are the ids that used to be rendered with leading 0s omitted. Setting this gate to false will force the reader to search for the spans with trace ids having leading zeroes"),
		featuregate.WithRegisterReferenceURL("https://github.com/jaegertracing/jaeger/issues/1578"))
}

// SpanReader can query for and load traces from ElasticSearch
type SpanReader struct {
	client func() es.Client
	// The age of the oldest service/operation we will look for. Because indices in ElasticSearch are by day,
	// this will be rounded down to UTC 00:00 of that day.
	maxSpanAge              time.Duration
	serviceOperationStorage *ServiceOperationStorage
	spanIndexPrefix         string
	serviceIndexPrefix      string
	spanIndex               cfg.IndexOptions
	serviceIndex            cfg.IndexOptions
	timeRangeIndices        TimeRangeIndexFn
	sourceFn                sourceFn
	maxDocCount             int
	useReadWriteAliases     bool
	logger                  *zap.Logger
	tracer                  trace.Tracer
	dotReplacer             dbmodel.DotReplacer
}

// SpanReaderParams holds constructor params for NewSpanReader
type SpanReaderParams struct {
	Client              func() es.Client
	MaxSpanAge          time.Duration
	MaxDocCount         int
	IndexPrefix         cfg.IndexPrefix
	SpanIndex           cfg.IndexOptions
	ServiceIndex        cfg.IndexOptions
	TagDotReplacement   string
	ReadAliasSuffix     string
	UseReadWriteAliases bool
	RemoteReadClusters  []string
	Logger              *zap.Logger
	Tracer              trace.Tracer
}

// NewSpanReader returns a new SpanReader with a metrics.
func NewSpanReader(p SpanReaderParams) *SpanReader {
	maxSpanAge := p.MaxSpanAge
	// Setting the maxSpanAge to a large duration will ensure all spans in the "read" alias are accessible by queries (query window = [now - maxSpanAge, now]).
	// When read/write aliases are enabled, which are required for index rollovers, only the "read" alias is queried and therefore should not affect performance.
	if p.UseReadWriteAliases {
		maxSpanAge = dawnOfTimeSpanAge
	}

	return &SpanReader{
		client:                  p.Client,
		maxSpanAge:              maxSpanAge,
		serviceOperationStorage: NewServiceOperationStorage(p.Client, p.Logger, 0), // the decorator takes care of metrics
		spanIndexPrefix:         p.IndexPrefix.Apply(spanIndexBaseName),
		serviceIndexPrefix:      p.IndexPrefix.Apply(serviceIndexBaseName),
		spanIndex:               p.SpanIndex,
		serviceIndex:            p.ServiceIndex,
		timeRangeIndices: LoggingTimeRangeIndexFn(
			p.Logger,
			TimeRangeIndicesFn(p.UseReadWriteAliases, p.ReadAliasSuffix, p.RemoteReadClusters),
		),
		sourceFn:            getSourceFn(p.MaxDocCount),
		maxDocCount:         p.MaxDocCount,
		useReadWriteAliases: p.UseReadWriteAliases,
		logger:              p.Logger,
		tracer:              p.Tracer,
		dotReplacer:         dbmodel.NewDotReplacer(p.TagDotReplacement),
	}
}

type TimeRangeIndexFn func(indexName string, indexDateLayout string, startTime time.Time, endTime time.Time, reduceDuration time.Duration) []string

type sourceFn func(query elastic.Query, nextTime uint64) *elastic.SearchSource

func LoggingTimeRangeIndexFn(logger *zap.Logger, fn TimeRangeIndexFn) TimeRangeIndexFn {
	if !logger.Core().Enabled(zap.DebugLevel) {
		return fn
	}
	return func(indexName string, indexDateLayout string, startTime time.Time, endTime time.Time, reduceDuration time.Duration) []string {
		indices := fn(indexName, indexDateLayout, startTime, endTime, reduceDuration)
		logger.Debug("Reading from ES indices", zap.Strings("index", indices))
		return indices
	}
}

func TimeRangeIndicesFn(useReadWriteAliases bool, readAliasSuffix string, remoteReadClusters []string) TimeRangeIndexFn {
	suffix := ""
	if useReadWriteAliases {
		if readAliasSuffix != "" {
			suffix = readAliasSuffix
		} else {
			suffix = "read"
		}
	}
	return addRemoteReadClusters(
		getTimeRangeIndexFn(useReadWriteAliases, suffix),
		remoteReadClusters,
	)
}

func getTimeRangeIndexFn(useReadWriteAliases bool, readAlias string) TimeRangeIndexFn {
	if useReadWriteAliases {
		return func(indexPrefix, _ /* indexDateLayout */ string, _ /* startTime */ time.Time, _ /* endTime */ time.Time, _ /* reduceDuration */ time.Duration) []string {
			return []string{indexPrefix + readAlias}
		}
	}
	return timeRangeIndices
}

// Add a remote cluster prefix for each cluster and for each index and add it to the list of original indices.
// Elasticsearch cross cluster api example GET /twitter,cluster_one:twitter,cluster_two:twitter/_search.
func addRemoteReadClusters(fn TimeRangeIndexFn, remoteReadClusters []string) TimeRangeIndexFn {
	return func(indexPrefix string, indexDateLayout string, startTime time.Time, endTime time.Time, reduceDuration time.Duration) []string {
		jaegerIndices := fn(indexPrefix, indexDateLayout, startTime, endTime, reduceDuration)
		if len(remoteReadClusters) == 0 {
			return jaegerIndices
		}

		for _, jaegerIndex := range jaegerIndices {
			for _, remoteCluster := range remoteReadClusters {
				remoteIndex := remoteCluster + ":" + jaegerIndex
				jaegerIndices = append(jaegerIndices, remoteIndex)
			}
		}

		return jaegerIndices
	}
}

func getSourceFn(maxDocCount int) sourceFn {
	return func(query elastic.Query, nextTime uint64) *elastic.SearchSource {
		return elastic.NewSearchSource().
			Query(query).
			Size(maxDocCount).
			Sort("startTime", true).
			SearchAfter(nextTime)
	}
}

// timeRangeIndices returns the array of indices that we need to query, based on query params
func timeRangeIndices(indexName, indexDateLayout string, startTime time.Time, endTime time.Time, reduceDuration time.Duration) []string {
	var indices []string
	firstIndex := indexWithDate(indexName, indexDateLayout, startTime)
	currentIndex := indexWithDate(indexName, indexDateLayout, endTime)
	for currentIndex != firstIndex && endTime.After(startTime) {
		if len(indices) == 0 || indices[len(indices)-1] != currentIndex {
			indices = append(indices, currentIndex)
		}
		endTime = endTime.Add(reduceDuration)
		currentIndex = indexWithDate(indexName, indexDateLayout, endTime)
	}
	indices = append(indices, firstIndex)
	return indices
}

// GetTraces takes a traceID and returns a Trace associated with that traceID
func (s *SpanReader) GetTraces(ctx context.Context, query []dbmodel.TraceID) ([]dbmodel.Trace, error) {
	ctx, span := s.tracer.Start(ctx, "GetTrace")
	defer span.End()
	currentTime := time.Now()
	// TODO: use start time & end time in "query" struct
	return s.multiRead(ctx, query, currentTime.Add(-s.maxSpanAge), currentTime)
}

func (s *SpanReader) collectSpans(esSpansRaw []*elastic.SearchHit) ([]dbmodel.Span, error) {
	spans := make([]dbmodel.Span, len(esSpansRaw))

	for i, esSpanRaw := range esSpansRaw {
		dbSpan, err := s.unmarshalJSONSpan(esSpanRaw)
		if err != nil {
			return nil, fmt.Errorf("marshalling JSON to span object failed: %w", err)
		}
		s.mergeAllNestedAndElevatedTagsOfSpan(&dbSpan)
		spans[i] = dbSpan
	}
	return spans, nil
}

func (*SpanReader) unmarshalJSONSpan(esSpanRaw *elastic.SearchHit) (dbmodel.Span, error) {
	esSpanInByteArray := esSpanRaw.Source

	var jsonSpan dbmodel.Span

	d := json.NewDecoder(bytes.NewReader(esSpanInByteArray))
	d.UseNumber()
	if err := d.Decode(&jsonSpan); err != nil {
		return dbmodel.Span{}, err
	}
	return jsonSpan, nil
}

// GetServices returns all services traced by Jaeger, ordered by frequency
func (s *SpanReader) GetServices(ctx context.Context) ([]string, error) {
	ctx, span := s.tracer.Start(ctx, "GetService")
	defer span.End()
	currentTime := time.Now()
	jaegerIndices := s.timeRangeIndices(
		s.serviceIndexPrefix,
		s.serviceIndex.DateLayout,
		currentTime.Add(-s.maxSpanAge),
		currentTime,
		cfg.RolloverFrequencyAsNegativeDuration(s.serviceIndex.RolloverFrequency),
	)
	return s.serviceOperationStorage.getServices(ctx, jaegerIndices, s.maxDocCount)
}

// GetOperations returns all operations for a specific service traced by Jaeger
func (s *SpanReader) GetOperations(
	ctx context.Context,
	query dbmodel.OperationQueryParameters,
) ([]dbmodel.Operation, error) {
	ctx, span := s.tracer.Start(ctx, "GetOperations")
	defer span.End()
	currentTime := time.Now()
	jaegerIndices := s.timeRangeIndices(
		s.serviceIndexPrefix,
		s.serviceIndex.DateLayout,
		currentTime.Add(-s.maxSpanAge),
		currentTime,
		cfg.RolloverFrequencyAsNegativeDuration(s.serviceIndex.RolloverFrequency),
	)
	operations, err := s.serviceOperationStorage.getOperations(ctx, jaegerIndices, query.ServiceName, s.maxDocCount)
	if err != nil {
		return nil, err
	}

	// TODO: https://github.com/jaegertracing/jaeger/issues/1923
	// 	- return the operations with actual span kind that meet requirement
	var result []dbmodel.Operation
	for _, operation := range operations {
		result = append(result, dbmodel.Operation{
			Name: operation,
		})
	}
	return result, err
}

func bucketToStringArray[T ~string](buckets []*elastic.AggregationBucketKeyItem) ([]T, error) {
	stringSlice := make([]T, len(buckets))
	for i, keyitem := range buckets {
		str, ok := keyitem.Key.(string)
		if !ok {
			return nil, errors.New("non-string key found in aggregation")
		}
		stringSlice[i] = T(str)
	}
	return stringSlice, nil
}

// FindTraces retrieves traces that match the traceQuery
func (s *SpanReader) FindTraces(ctx context.Context, traceQuery dbmodel.TraceQueryParameters) ([]dbmodel.Trace, error) {
	ctx, span := s.tracer.Start(ctx, "FindTraces")
	defer span.End()

	uniqueTraceIDs, err := s.FindTraceIDs(ctx, traceQuery)
	if err != nil {
		return nil, es.DetailedError(err)
	}
	return s.multiRead(ctx, uniqueTraceIDs, traceQuery.StartTimeMin, traceQuery.StartTimeMax)
}

// FindTraceIDs retrieves traces IDs that match the traceQuery
func (s *SpanReader) FindTraceIDs(ctx context.Context, traceQuery dbmodel.TraceQueryParameters) ([]dbmodel.TraceID, error) {
	ctx, span := s.tracer.Start(ctx, "FindTraceIDs")
	defer span.End()

	if err := validateQuery(traceQuery); err != nil {
		return nil, err
	}
	if traceQuery.NumTraces == 0 {
		traceQuery.NumTraces = defaultNumTraces
	}

	esTraceIDs, err := s.findTraceIDs(ctx, traceQuery)
	if err != nil {
		return nil, err
	}

	return esTraceIDs, nil
}

func (s *SpanReader) multiRead(ctx context.Context, traceIDs []dbmodel.TraceID, startTime, endTime time.Time) ([]dbmodel.Trace, error) {
	ctx, childSpan := s.tracer.Start(ctx, "multiRead")
	defer childSpan.End()

	if childSpan.IsRecording() {
		tracesIDs := make([]string, len(traceIDs))
		for i, traceID := range traceIDs {
			tracesIDs[i] = string(traceID)
		}
		childSpan.SetAttributes(attribute.Key("trace_ids").StringSlice(tracesIDs))
	}

	traces := make([]dbmodel.Trace, 0, len(traceIDs))

	if len(traceIDs) == 0 {
		return traces, nil
	}

	// Add an hour in both directions so that traces that straddle two indexes are retrieved.
	// i.e starts in one and ends in another.
	indices := s.timeRangeIndices(
		s.spanIndexPrefix,
		s.spanIndex.DateLayout,
		startTime.Add(-time.Hour),
		endTime.Add(time.Hour),
		cfg.RolloverFrequencyAsNegativeDuration(s.spanIndex.RolloverFrequency),
	)
	nextTime := model.TimeAsEpochMicroseconds(startTime.Add(-time.Hour))
	searchAfterTime := make(map[dbmodel.TraceID]uint64)
	totalDocumentsFetched := make(map[dbmodel.TraceID]int)
	tracesMap := make(map[dbmodel.TraceID]*dbmodel.Trace)
	for {
		if len(traceIDs) == 0 {
			break
		}
		searchRequests := make([]*elastic.SearchRequest, len(traceIDs))
		for i, traceID := range traceIDs {
			traceQuery := buildTraceByIDQuery(traceID)
			query := elastic.NewBoolQuery().
				Must(traceQuery)
			if s.useReadWriteAliases {
				startTimeRangeQuery := s.buildStartTimeQuery(startTime.Add(-time.Hour*24), endTime.Add(time.Hour*24))
				query = query.Must(startTimeRangeQuery)
			}

			if val, ok := searchAfterTime[traceID]; ok {
				nextTime = val
			}

			s := s.sourceFn(query, nextTime).
				TrackTotalHits(true)
			searchRequests[i] = elastic.NewSearchRequest().
				IgnoreUnavailable(true).
				Source(s)
		}
		// set traceIDs to empty
		traceIDs = nil
		results, err := s.client().MultiSearch().Add(searchRequests...).Index(indices...).Do(ctx)
		if err != nil {
			err = es.DetailedError(err)
			logErrorToSpan(childSpan, err)
			return nil, err
		}

		if len(results.Responses) == 0 {
			break
		}

		for _, result := range results.Responses {
			if result.Hits == nil || len(result.Hits.Hits) == 0 {
				continue
			}
			spans, err := s.collectSpans(result.Hits.Hits)
			if err != nil {
				err = es.DetailedError(err)
				logErrorToSpan(childSpan, err)
				return nil, err
			}
			lastSpan := spans[len(spans)-1]

			if traceSpan, ok := tracesMap[lastSpan.TraceID]; ok {
				traceSpan.Spans = append(traceSpan.Spans, spans...)
			} else {
				traces = append(traces, dbmodel.Trace{Spans: spans})
				tracesMap[lastSpan.TraceID] = &traces[len(traces)-1]
			}

			totalDocumentsFetched[lastSpan.TraceID] += len(result.Hits.Hits)
			if totalDocumentsFetched[lastSpan.TraceID] < int(result.TotalHits()) {
				traceIDs = append(traceIDs, lastSpan.TraceID)
				searchAfterTime[lastSpan.TraceID] = lastSpan.StartTime
			}
		}
	}
	return traces, nil
}

func buildTraceByIDQuery(traceID dbmodel.TraceID) elastic.Query {
	traceIDStr := string(traceID)
	if traceIDStr[0] != '0' || disableLegacyIDs.IsEnabled() {
		return elastic.NewTermQuery(traceIDField, traceIDStr)
	}
	// https://github.com/jaegertracing/jaeger/pull/1956 added leading zeros to IDs
	// So we need to also read IDs without leading zeros for compatibility with previously saved data.
	legacyTraceID := strings.TrimLeft(traceIDStr, "0")
	return elastic.NewBoolQuery().Should(
		elastic.NewTermQuery(traceIDField, traceIDStr).Boost(2),
		elastic.NewTermQuery(traceIDField, legacyTraceID))
}

func validateQuery(p dbmodel.TraceQueryParameters) error {
	if p.ServiceName == "" && len(p.Tags) > 0 {
		return ErrServiceNameNotSet
	}
	if p.StartTimeMin.IsZero() || p.StartTimeMax.IsZero() {
		return ErrStartAndEndTimeNotSet
	}
	if p.StartTimeMax.Before(p.StartTimeMin) {
		return ErrStartTimeMinGreaterThanMax
	}
	if p.DurationMin != 0 && p.DurationMax != 0 && p.DurationMin > p.DurationMax {
		return ErrDurationMinGreaterThanMax
	}
	return nil
}

func (s *SpanReader) findTraceIDs(ctx context.Context, traceQuery dbmodel.TraceQueryParameters) ([]dbmodel.TraceID, error) {
	ctx, childSpan := s.tracer.Start(ctx, "findTraceIDs")
	defer childSpan.End()
	//  Below is the JSON body to our HTTP GET request to ElasticSearch. This function creates this.
	// {
	//      "size": 0,
	//      "query": {
	//        "bool": {
	//          "must": [
	//            { "match": { "operationName":   "op1"      }},
	//            { "match": { "process.serviceName": "service1" }},
	//            { "range":  { "startTime": { "gte": 0, "lte": 90000000000000000 }}},
	//            { "range":  { "duration": { "gte": 0, "lte": 90000000000000000 }}},
	//            { "should": [
	//                   { "nested" : {
	//                      "path" : "tags",
	//                      "query" : {
	//                          "bool" : {
	//                              "must" : [
	//                              { "match" : {"tags.key" : "tag3"} },
	//                              { "match" : {"tags.value" : "xyz"} }
	//                              ]
	//                          }}}},
	//                   { "nested" : {
	//                          "path" : "process.tags",
	//                          "query" : {
	//                              "bool" : {
	//                                  "must" : [
	//                                  { "match" : {"tags.key" : "tag3"} },
	//                                  { "match" : {"tags.value" : "xyz"} }
	//                                  ]
	//                              }}}},
	//                   { "nested" : {
	//                          "path" : "logs.fields",
	//                          "query" : {
	//                              "bool" : {
	//                                  "must" : [
	//                                  { "match" : {"tags.key" : "tag3"} },
	//                                  { "match" : {"tags.value" : "xyz"} }
	//                                  ]
	//                              }}}},
	//                   { "bool":{
	//                           "must": {
	//                               "match":{ "tags.bat":{ "query":"spook" }}
	//                           }}},
	//                   { "bool":{
	//                           "must": {
	//                               "match":{ "tag.bat":{ "query":"spook" }}
	//                           }}}
	//                ]
	//              }
	//          ]
	//        }
	//      },
	//      "aggs": { "traceIDs" : { "terms" : {"size": 100,"field": "traceID" }}}
	//  }
	aggregation := s.buildTraceIDAggregation(traceQuery.NumTraces)
	boolQuery := s.buildFindTraceIDsQuery(traceQuery)
	jaegerIndices := s.timeRangeIndices(
		s.spanIndexPrefix,
		s.spanIndex.DateLayout,
		traceQuery.StartTimeMin,
		traceQuery.StartTimeMax,
		cfg.RolloverFrequencyAsNegativeDuration(s.spanIndex.RolloverFrequency),
	)

	searchService := s.client().Search(jaegerIndices...).
		Size(0). // set to 0 because we don't want actual documents.
		Aggregation(traceIDAggregation, aggregation).
		IgnoreUnavailable(true).
		Query(boolQuery)

	searchResult, err := searchService.Do(ctx)
	if err != nil {
		err = es.DetailedError(err)
		s.logger.Info("es search services failed", zap.Any("traceQuery", traceQuery), zap.Error(err))
		return nil, fmt.Errorf("search services failed: %w", err)
	}
	if searchResult.Aggregations == nil {
		return []dbmodel.TraceID{}, nil
	}
	bucket, found := searchResult.Aggregations.Terms(traceIDAggregation)
	if !found {
		return nil, ErrUnableToFindTraceIDAggregation
	}

	traceIDBuckets := bucket.Buckets
	return bucketToStringArray[dbmodel.TraceID](traceIDBuckets)
}

func (s *SpanReader) buildTraceIDAggregation(numOfTraces int) elastic.Aggregation {
	return elastic.NewTermsAggregation().
		Size(numOfTraces).
		Field(traceIDField).
		Order(startTimeField, false).
		SubAggregation(startTimeField, s.buildTraceIDSubAggregation())
}

func (*SpanReader) buildTraceIDSubAggregation() elastic.Aggregation {
	return elastic.NewMaxAggregation().
		Field(startTimeField)
}

func (s *SpanReader) buildFindTraceIDsQuery(traceQuery dbmodel.TraceQueryParameters) elastic.Query {
	boolQuery := elastic.NewBoolQuery()

	// add duration query
	if traceQuery.DurationMax != 0 || traceQuery.DurationMin != 0 {
		durationQuery := s.buildDurationQuery(traceQuery.DurationMin, traceQuery.DurationMax)
		boolQuery.Must(durationQuery)
	}

	// add startTime query
	startTimeQuery := s.buildStartTimeQuery(traceQuery.StartTimeMin, traceQuery.StartTimeMax)
	boolQuery.Must(startTimeQuery)

	// add process.serviceName query
	if traceQuery.ServiceName != "" {
		serviceNameQuery := s.buildServiceNameQuery(traceQuery.ServiceName)
		boolQuery.Must(serviceNameQuery)
	}

	// add operationName query
	if traceQuery.OperationName != "" {
		operationNameQuery := s.buildOperationNameQuery(traceQuery.OperationName)
		boolQuery.Must(operationNameQuery)
	}

	for k, v := range traceQuery.Tags {
		tagQuery := s.buildTagQuery(k, v)
		boolQuery.Must(tagQuery)
	}
	return boolQuery
}

func (*SpanReader) buildDurationQuery(durationMin time.Duration, durationMax time.Duration) elastic.Query {
	minDurationMicros := model.DurationAsMicroseconds(durationMin)
	maxDurationMicros := defaultMaxDuration
	if durationMax != 0 {
		maxDurationMicros = model.DurationAsMicroseconds(durationMax)
	}
	return elastic.NewRangeQuery(durationField).Gte(minDurationMicros).Lte(maxDurationMicros)
}

func (*SpanReader) buildStartTimeQuery(startTimeMin time.Time, startTimeMax time.Time) elastic.Query {
	minStartTimeMicros := model.TimeAsEpochMicroseconds(startTimeMin)
	maxStartTimeMicros := model.TimeAsEpochMicroseconds(startTimeMax)
	// startTimeMillisField is date field in ES mapping.
	// Using date field in range queries helps to skip search on unnecessary shards at Elasticsearch side.
	// https://discuss.elastic.co/t/timeline-query-on-timestamped-indices/129328/2
	return elastic.NewRangeQuery(startTimeMillisField).Gte(minStartTimeMicros / 1000).Lte(maxStartTimeMicros / 1000)
}

func (*SpanReader) buildServiceNameQuery(serviceName string) elastic.Query {
	return elastic.NewMatchQuery(serviceNameField, serviceName)
}

func (*SpanReader) buildOperationNameQuery(operationName string) elastic.Query {
	return elastic.NewMatchQuery(operationNameField, operationName)
}

func (s *SpanReader) buildTagQuery(k string, v string) elastic.Query {
	objectTagListLen := len(objectTagFieldList)
	queries := make([]elastic.Query, len(nestedTagFieldList)+objectTagListLen)
	kd := s.dotReplacer.ReplaceDot(k)
	for i := range objectTagFieldList {
		queries[i] = s.buildObjectQuery(objectTagFieldList[i], kd, v)
	}
	for i := range nestedTagFieldList {
		queries[i+objectTagListLen] = s.buildNestedQuery(nestedTagFieldList[i], k, v)
	}

	// but configuration can change over time
	return elastic.NewBoolQuery().Should(queries...)
}

func (*SpanReader) buildNestedQuery(field string, k string, v string) elastic.Query {
	keyField := fmt.Sprintf("%s.%s", field, tagKeyField)
	valueField := fmt.Sprintf("%s.%s", field, tagValueField)
	keyQuery := elastic.NewMatchQuery(keyField, k)
	valueQuery := elastic.NewRegexpQuery(valueField, v)
	tagBoolQuery := elastic.NewBoolQuery().Must(keyQuery, valueQuery)
	return elastic.NewNestedQuery(field, tagBoolQuery)
}

func (*SpanReader) buildObjectQuery(field string, k string, v string) elastic.Query {
	keyField := fmt.Sprintf("%s.%s", field, k)
	keyQuery := elastic.NewRegexpQuery(keyField, v)
	return elastic.NewBoolQuery().Must(keyQuery)
}

func (s *SpanReader) mergeAllNestedAndElevatedTagsOfSpan(span *dbmodel.Span) {
	processTags := s.mergeNestedAndElevatedTags(span.Process.Tags, span.Process.Tag)
	span.Process.Tags = processTags
	spanTags := s.mergeNestedAndElevatedTags(span.Tags, span.Tag)
	span.Tags = spanTags
}

func (s *SpanReader) mergeNestedAndElevatedTags(nestedTags []dbmodel.KeyValue, elevatedTags map[string]any) []dbmodel.KeyValue {
	mergedTags := make([]dbmodel.KeyValue, 0, len(nestedTags)+len(elevatedTags))
	mergedTags = append(mergedTags, nestedTags...)
	for k, v := range elevatedTags {
		kv := s.convertTagField(k, v)
		mergedTags = append(mergedTags, kv)
		delete(elevatedTags, k)
	}
	return mergedTags
}

func (s *SpanReader) convertTagField(k string, v any) dbmodel.KeyValue {
	dKey := s.dotReplacer.ReplaceDotReplacement(k)
	kv := dbmodel.KeyValue{
		Key:   dKey,
		Value: v,
	}
	switch val := v.(type) {
	case int64:
		kv.Type = dbmodel.Int64Type
	case float64:
		kv.Type = dbmodel.Float64Type
	case bool:
		kv.Type = dbmodel.BoolType
	case string:
		kv.Type = dbmodel.StringType
	// the binary is never returned, ES returns it as string with base64 encoding
	case []byte:
		kv.Type = dbmodel.BinaryType
	// in spans are decoded using json.UseNumber() to preserve the type
	// however note that float(1) will be parsed as int as ES does not store decimal point
	case json.Number:
		n, err := val.Int64()
		if err == nil {
			kv.Value = n
			kv.Type = dbmodel.Int64Type
		} else {
			f, err := val.Float64()
			if err != nil {
				return dbmodel.KeyValue{
					Key:   dKey,
					Value: fmt.Sprintf("invalid tag type in %+v: %s", v, err.Error()),
					Type:  dbmodel.StringType,
				}
			}
			kv.Value = f
			kv.Type = dbmodel.Float64Type
		}
	default:
		return dbmodel.KeyValue{
			Key:   dKey,
			Value: fmt.Sprintf("invalid tag type in %+v", v),
			Type:  dbmodel.StringType,
		}
	}
	return kv
}

func logErrorToSpan(span trace.Span, err error) {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}
