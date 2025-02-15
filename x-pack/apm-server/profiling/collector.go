// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

package profiling

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/golang/protobuf/ptypes/empty"
	"github.com/hashicorp/golang-lru/simplelru"
	"google.golang.org/grpc/codes"
	_ "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/monitoring"
	"github.com/elastic/go-elasticsearch/v8/esutil"

	"github.com/elastic/apm-server/x-pack/apm-server/profiling/common"
	"github.com/elastic/apm-server/x-pack/apm-server/profiling/libpf"
)

var (
	// metrics
	indexerDocs                 = monitoring.Default.NewRegistry("apm-server.profiling.indexer.document")
	counterEventsTotal          = monitoring.NewInt(indexerDocs, "events.total.count")
	counterEventsFailure        = monitoring.NewInt(indexerDocs, "events.failure.count")
	counterStacktracesTotal     = monitoring.NewInt(indexerDocs, "stacktraces.total.count")
	counterStacktracesDuplicate = monitoring.NewInt(indexerDocs, "stacktraces.duplicate.count")
	counterStacktracesFailure   = monitoring.NewInt(indexerDocs, "stacktraces.failure.count")
	counterStackframesTotal     = monitoring.NewInt(indexerDocs, "stackframes.total.count")
	counterStackframesDuplicate = monitoring.NewInt(indexerDocs, "stackframes.duplicate.count")
	counterStackframesFailure   = monitoring.NewInt(indexerDocs, "stackframes.failure.count")
	counterExecutablesTotal     = monitoring.NewInt(indexerDocs, "executables.total.count")
	counterExecutablesFailure   = monitoring.NewInt(indexerDocs, "executables.failure.count")

	counterFatalErr = monitoring.NewInt(nil, "apm-server.profiling.unrecoverable_error.count")

	// gRPC error returned to the clients
	errCustomer = status.Error(codes.Internal, "failed to process request")
)

const (
	actionIndex  = "index"
	actionCreate = "create"
	actionUpdate = "update"

	sourceFileCacheSize = 128 * 1024
	// ES error string indicating a duplicate document by _id
	docIDAlreadyExists = "version_conflict_engine_exception"
)

// ElasticCollector is an implementation of the gRPC server handling the data
// sent by Host-Agent.
type ElasticCollector struct {
	// See https://github.com/grpc/grpc-go/issues/3669 for why this struct is embedded.
	UnimplementedCollectionAgentServer

	logger         *logp.Logger
	indexer        esutil.BulkIndexer
	metricsIndexer esutil.BulkIndexer
	indexes        [common.MaxEventsIndexes]string

	sourceFilesLock sync.Mutex
	sourceFiles     *simplelru.LRU
	clusterID       string

	fileIDQueue    *SymQueue[libpf.FileID]
	leafFrameQueue *SymQueue[common.FrameID]
}

// NewCollector returns a new ElasticCollector which uses indexer for storing stack trace
// data in Elasticsearch, and metricsIndexer for storing host agent metrics. Separate
// indexers are used to allow for host agent metrics to be sent to a separate monitoring
// cluster.
func NewCollector(
	indexer esutil.BulkIndexer,
	metricsIndexer esutil.BulkIndexer,
	esClusterID string,
	logger *logp.Logger,
) *ElasticCollector {
	sourceFiles, err := simplelru.NewLRU(sourceFileCacheSize, nil)
	if err != nil {
		log.Fatalf("Failed to create source file LRU: %v", err)
	}

	c := &ElasticCollector{
		logger:         logger,
		indexer:        indexer,
		metricsIndexer: metricsIndexer,
		sourceFiles:    sourceFiles,
		clusterID:      esClusterID,
	}

	queueConfig := DefaultQueueConfig()
	queueConfig.Size = 8
	queueConfig.CacheSize = 10240
	c.fileIDQueue = NewQueue(queueConfig, c.flushExecutablesForSymbolization)
	queueConfig.Size = 1024
	queueConfig.CacheSize = 131072
	c.leafFrameQueue = NewQueue(queueConfig, c.flushLeafFramesForSymbolization)

	// Precalculate index names to minimise per-TraceEvent overhead.
	for i := range c.indexes {
		c.indexes[i] = fmt.Sprintf("%s-%dpow%02d", common.EventsIndexPrefix,
			common.SamplingFactor, i+1)
	}

	rpcProtocolVersion = GetRPCVersionFromProto()
	return c
}

// SaveHostInfo is deprecated and not used in 8.8+, but the stub still exists here
// in order to trigger "upgrade HA, incompatible protocol" user-facing error and
// stop HA execution in environments still running 8.7 clients.
func (*ElasticCollector) SaveHostInfo(context.Context, *HostInfo) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

// Heartbeat is deprecated and not used in 8.8+, but the stub still exists here
// in order to trigger "upgrade HA, incompatible protocol" user-facing error and
// stop HA execution in environments still running 8.7 clients.
func (*ElasticCollector) Heartbeat(context.Context, *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

// AddCountsForTraces implements the RPC to send stacktrace data: stacktrace hashes and counts.
func (e *ElasticCollector) AddCountsForTraces(ctx context.Context,
	req *AddCountsForTracesRequest) (*emptypb.Empty, error) {
	traceEvents, err := mapToStackTraceEvents(ctx, req)
	if err != nil {
		e.logger.With(
			logp.Error(err),
			logp.String("grpc_method", "AddCountsForTraces"),
		).Error("error mapping host-agent traces to Elastic stacktraces")
		return nil, errCustomer
	}
	counterEventsTotal.Add(int64(len(traceEvents)))

	// Store every event as-is into the full events index.
	e.logger.With(
		logp.String("grpc_method", "AddCountsForTraces"),
	).Infof("adding %d trace events", len(traceEvents))
	for i := range traceEvents {
		if err := e.indexStacktrace(ctx, &traceEvents[i], common.AllEventsIndex); err != nil {
			e.logger.With(
				logp.Error(err),
				logp.String("grpc_method", "AddCountsForTraces"),
			).Error("Elasticsearch indexing error")
			return nil, errCustomer
		}
	}

	// Each event has a probability of p=1/5=0.2 to go from one index into the next downsampled
	// index. Since we aggregate identical stacktrace events by timestamp when reported and stored,
	// we have a 'Count' value for each. To be statistically correct, we have to apply p=0.2 to
	// each single stacktrace event independently and not just to the aggregate. We can do so by
	// looping over 'Count' and apply p=0.2 on every iteration to generate a new 'Count' value for
	// the next downsampled index.
	// We only store aggregates with 'Count' > 0. If 'Count' becomes 0, we are done and can
	// continue with the next stacktrace event.
	for i := range traceEvents {
		for _, index := range e.indexes {
			count := uint16(0)
			for j := uint16(0); j < traceEvents[i].Count; j++ {
				// samplingRatio is the probability p=0.2 for an event to be copied into the next
				// downsampled index.
				if rand.Float64() < common.SamplingRatio { //nolint:gosec
					count++
				}
			}
			if count == 0 {
				// We are done with this event, process the next one.
				break
			}

			// Store the event with its new downsampled count in the downsampled index.
			traceEvents[i].Count = count

			if err := e.indexStacktrace(ctx, &traceEvents[i], index); err != nil {
				e.logger.With(
					logp.Error(err),
					logp.String("grpc_method", "AddCountsForTraces"),
				).Error("Elasticsearch indexing error")
				return nil, errCustomer
			}
		}
	}

	return &emptypb.Empty{}, nil
}

func (e *ElasticCollector) indexStacktrace(ctx context.Context, traceEvent *StackTraceEvent,
	indexName string) (err error) {
	body, err := common.EncodeBody(traceEvent)
	if err != nil {
		return err
	}

	return e.indexer.Add(ctx, esutil.BulkIndexerItem{
		Index:  indexName,
		Action: actionCreate,
		Body:   body,
		OnFailure: func(
			_ context.Context,
			_ esutil.BulkIndexerItem,
			resp esutil.BulkIndexerResponseItem,
			err error,
		) {
			counterEventsFailure.Inc()
			e.logger.With(
				logp.Error(err),
				logp.String("index", indexName),
				logp.String("error_type", resp.Error.Type),
			).Errorf("failed to index stacktrace event: %s", resp.Error.Reason)
		},
	})
}

// StackTraceEvent represents a stacktrace event serializable into ES.
// The json field names need to be case-sensitively equal to the fields defined
// in the schema mapping.
type StackTraceEvent struct {
	common.EcsVersion
	ProjectID uint32 `json:"service.name"`
	TimeStamp uint32 `json:"@timestamp"`
	HostID    uint64 `json:"host.id"`
	// 128-bit hash in binary form
	StackTraceID  string `json:"Stacktrace.id"`
	PodName       string `json:"orchestrator.resource.name,omitempty"`
	ContainerName string `json:"container.name,omitempty"`
	ThreadName    string `json:"process.thread.name"`
	Count         uint16 `json:"Stacktrace.count"`

	// Host metadata
	Tags []string `json:"tags,omitempty"`
	// HostIP is the list of network cards IPs, mapped to an Elasticsearch "ip" data type field
	HostIP       []string `json:"host.ip,omitempty"`
	HostName     string   `json:"host.name,omitempty"`
	OSKernel     string   `json:"os.kernel,omitempty"`
	AgentVersion string   `json:"agent.version,omitempty"`
}

// StackTrace represents a stacktrace serializable into the stacktraces index.
// DocID should be the base64-encoded Stacktrace ID.
type StackTrace struct {
	common.EcsVersion
	FrameIDs string `json:"Stacktrace.frame.ids"`
	Types    string `json:"Stacktrace.frame.types"`
}

// StackFrame represents a stacktrace serializable into the stackframes index.
// DocID should be the base64-encoded FileID+Address (24 bytes).
type StackFrame struct {
	common.EcsVersion
	FileName       string `json:"Stackframe.file.name,omitempty"`
	FunctionName   string `json:"Stackframe.function.name,omitempty"`
	LineNumber     int32  `json:"Stackframe.line.number,omitempty"`
	FunctionOffset int32  `json:"Stackframe.function.offset,omitempty"`
}

// mapToStackTraceEvents maps Prodfiler stacktraces to Elastic documents.
func mapToStackTraceEvents(ctx context.Context,
	req *AddCountsForTracesRequest) ([]StackTraceEvent, error) {
	traces, err := CollectTracesAndCounts(req)
	if err != nil {
		return nil, err
	}

	projectID := GetProjectID(ctx)
	hostID := GetHostID(ctx)
	kernelVersion := GetKernelVersion(ctx)
	hostName := GetHostname(ctx)
	agentVersion := GetRevision(ctx)

	tags := strings.Split(GetTags(ctx), ";")
	if len(tags) == 1 && tags[0] == "" {
		// prevent storing 'tags'
		tags = nil
	}

	ipAddress := GetIPAddress(ctx)
	ipAddresses := []string{ipAddress}
	if ipAddress == "" {
		// prevent storing 'host.ip'
		ipAddresses = nil
	}

	traceEvents := make([]StackTraceEvent, 0, len(traces))
	for i := range traces {
		traceEvents = append(traceEvents,
			StackTraceEvent{
				ProjectID:     projectID,
				TimeStamp:     uint32(traces[i].Timestamp),
				HostID:        hostID,
				StackTraceID:  common.EncodeStackTraceID(traces[i].Hash),
				PodName:       traces[i].PodName,
				ContainerName: traces[i].ContainerName,
				ThreadName:    traces[i].Comm,
				Count:         traces[i].Count,
				Tags:          tags,
				HostIP:        ipAddresses,
				HostName:      hostName,
				OSKernel:      kernelVersion,
				AgentVersion:  agentVersion,
			})
	}

	return traceEvents, nil
}

// Script written in Painless that will both create a new document (if DocID does not exist),
// and update timestamp of an existing document. Named parameters are used to improve performance
// re: script compilation (since the script does not change across executions, it can be compiled
// once and cached).
const exeMetadataUpsertScript = `
if (ctx.op == 'create') {
    ctx._source['@timestamp']            = params.timestamp;
    ctx._source['Executable.build.id']   = params.buildid;
    ctx._source['Executable.file.name']  = params.filename;
    ctx._source['ecs.version']           = params.ecsversion;
} else {
    if (ctx._source['@timestamp'] == params.timestamp) {
        ctx.op = 'noop'
    } else {
        ctx._source['@timestamp'] = params.timestamp
    }
}
`

type ExeMetadataScript struct {
	Source string            `json:"source"`
	Params ExeMetadataParams `json:"params"`
}

type ExeMetadataParams struct {
	LastSeen   uint32 `json:"timestamp"`
	BuildID    string `json:"buildid"`
	FileName   string `json:"filename"`
	EcsVersion string `json:"ecsversion"`
}

// ExeMetadata represents executable metadata serializable into the executables index.
// DocID should be the base64-encoded FileID.
type ExeMetadata struct {
	// ScriptedUpsert needs to be 'true' for the script to execute regardless of the
	// document existing or not.
	ScriptedUpsert bool              `json:"scripted_upsert"`
	Script         ExeMetadataScript `json:"script"`
	// This needs to exist for document creation to succeed (if document does not exist),
	// but can be empty as the script implements both document creation and updating.
	Upsert struct{} `json:"upsert"`
}

func (e *ElasticCollector) AddExecutableMetadata(ctx context.Context,
	in *AddExecutableMetadataRequest) (*empty.Empty, error) {
	hiFileIDs := in.GetHiFileIDs()
	loFileIDs := in.GetLoFileIDs()

	numHiFileIDs := len(hiFileIDs)
	numLoFileIDs := len(loFileIDs)

	if numHiFileIDs == 0 {
		e.logger.With(
			logp.String("grpc_method", "AddExecutableMetadata"),
		).Debug("request with no entries")
		return &empty.Empty{}, nil
	}

	// Sanity check. Should never happen unless the HA is broken.
	if numHiFileIDs != numLoFileIDs {
		e.logger.With(
			logp.String("grpc_method", "AddExecutableMetadata"),
		).Errorf("mismatch in number of file IDAs (%d) file IDBs (%d)",
			numHiFileIDs, numLoFileIDs)
		counterFatalErr.Inc()
		return nil, errCustomer
	}

	counterExecutablesTotal.Add(int64(numHiFileIDs))

	filenames := in.GetFilenames()
	buildIDs := in.GetBuildIDs()

	lastSeen := common.GetStartOfWeekFromTime(time.Now())

	for i := 0; i < numHiFileIDs; i++ {
		fileID := libpf.NewFileID(hiFileIDs[i], loFileIDs[i])

		body, err := common.EncodeBodyBytes(ExeMetadata{
			ScriptedUpsert: true,
			Script: ExeMetadataScript{
				Source: exeMetadataUpsertScript,
				Params: ExeMetadataParams{
					LastSeen:   lastSeen,
					BuildID:    buildIDs[i],
					FileName:   filenames[i],
					EcsVersion: common.EcsVersionString,
				},
			},
		})
		if err != nil {
			e.logger.With(
				logp.Error(err),
				logp.String("grpc_method", "AddExecutableMetadata"),
			).Error("failed to JSON encode executable")
			return nil, errCustomer
		}

		// DocID is the base64-encoded FileID.
		docID := common.EncodeFileID(fileID)

		err = multiplexCurrentNextIndicesWrite(ctx, e, &esutil.BulkIndexerItem{
			Index:      common.ExecutablesIndex,
			Action:     actionUpdate,
			DocumentID: docID,
			OnFailure: func(
				_ context.Context,
				_ esutil.BulkIndexerItem,
				resp esutil.BulkIndexerResponseItem,
				err error,
			) {
				counterExecutablesFailure.Inc()
				e.logger.With(
					logp.Error(err),
					logp.String("error_type", resp.Error.Type),
					logp.String("grpc_method", "AddExecutableMetadata"),
				).Errorf("failed to index executable metadata: %s", resp.Error.Reason)
			},
		}, body)
		if err != nil {
			e.logger.With(
				logp.Error(err),
				logp.String("grpc_method", "AddExecutableMetadata"),
			).Error("Elasticsearch indexing error")
			return nil, errCustomer
		}
	}

	return &emptypb.Empty{}, nil
}

// ReportHostMetadata is needed too otherwise host-agent will not start properly
func (*ElasticCollector) ReportHostMetadata(context.Context,
	*HostMetadata) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

// ExecutableSymbolizationData represents an array of executable FileIDs written into the
// executable symbolization queue index.
type ExecutableSymbolizationData struct {
	common.EcsVersion
	FileID  []string  `json:"Executable.file.id"`
	Created time.Time `json:"Time.created"`
	Next    time.Time `json:"Symbolization.time.next"`
	Retries int       `json:"Symbolization.retries"`
}

func (e *ElasticCollector) flushExecutablesForSymbolization(ctx context.Context,
	fileIDs []libpf.FileID) {
	e.logger.With(
		logp.String("method", "flushExecutablesForSymbolization"),
	).Infof("Flush %d executables", len(fileIDs))

	fileIDStrings := make([]string, len(fileIDs))
	for i := 0; i < len(fileIDs); i++ {
		fileIDStrings[i] = common.EncodeFileID(fileIDs[i])
	}

	now := time.Now()
	body, err := common.EncodeBody(ExecutableSymbolizationData{
		FileID:  fileIDStrings,
		Created: now,
		Next:    now,
		Retries: 0,
	})
	if err != nil {
		e.logger.With(
			logp.Error(err),
			logp.String("method", "flushExecutablesForSymbolization"),
		).Error("Failed to JSON encode executables")
		return
	}

	err = e.indexer.Add(ctx, esutil.BulkIndexerItem{
		Index:  common.ExecutablesSymQueueIndex,
		Action: actionIndex,
		Body:   body,
		OnFailure: func(ctx context.Context, _ esutil.BulkIndexerItem,
			resp esutil.BulkIndexerResponseItem, err error) {
			e.logger.With(
				logp.Error(err),
				logp.String("method", "flushExecutablesForSymbolization"),
			).Errorf("Failed to index document: %#v", resp.Error)
		},
	})
	if err != nil {
		e.logger.With(
			logp.Error(err),
			logp.String("method", "flushExecutablesForSymbolization"),
		).Error("Elasticsearch indexing error")
	}
}

// LeafFrameSymbolizationData represents an array of frame IDs written into the
// leaf frame symbolization queue index.
type LeafFrameSymbolizationData struct {
	common.EcsVersion
	FrameID []string  `json:"Stacktrace.frame.id"`
	Created time.Time `json:"Time.created"`
	Next    time.Time `json:"Symbolization.time.next"`
	Retries int       `json:"Symbolization.retries"`
}

func (e *ElasticCollector) flushLeafFramesForSymbolization(ctx context.Context,
	leafFrames []common.FrameID) {
	if len(leafFrames) == 0 {
		// The queue doesn't flush empty arrays, but let's make sure.
		return
	}

	// Order the leaf frames by fileID.
	sort.Slice(leafFrames, func(i, j int) bool {
		return bytes.Compare(leafFrames[i].FileIDBytes(), leafFrames[j].FileIDBytes()) < 0
	})

	// Write leaf frames grouped by fileID.
	// This is very *important* as the symbolization service relies on it.
	pos := 0
	key := leafFrames[0].FileIDBytes()
	for i := 1; i < len(leafFrames); i++ {
		if !bytes.Equal(key, leafFrames[i].FileIDBytes()) {
			e.writeLeafFramesForSymbolization(ctx, leafFrames[pos:i])
			pos = i
			key = leafFrames[i].FileIDBytes()
		}
	}
	e.writeLeafFramesForSymbolization(ctx, leafFrames[pos:])
}

func (e *ElasticCollector) writeLeafFramesForSymbolization(ctx context.Context,
	leafFrames []common.FrameID) {
	leafFrameStrings := make([]string, len(leafFrames))
	for i := 0; i < len(leafFrames); i++ {
		leafFrameStrings[i] = base64.RawURLEncoding.EncodeToString(leafFrames[i].Bytes())
	}

	now := time.Now()
	body, err := common.EncodeBody(LeafFrameSymbolizationData{
		FrameID: leafFrameStrings,
		Created: now,
		Next:    now,
		Retries: 0,
	})
	if err != nil {
		return
	}

	err = e.indexer.Add(ctx, esutil.BulkIndexerItem{
		Index:  common.LeafFramesSymQueueIndex,
		Action: actionIndex,
		Body:   body,
		OnFailure: func(ctx context.Context, _ esutil.BulkIndexerItem,
			resp esutil.BulkIndexerResponseItem, err error) {
			e.logger.With(
				logp.Error(err),
				logp.String("method", "flushLeafFramesForSymbolization"),
			).Errorf("Failed to index document: %#v", resp.Error)
		},
	})
	if err != nil {
		e.logger.With(
			logp.Error(err),
			logp.String("method", "flushLeafFramesForSymbolization"),
		).Error("Elasticsearch indexing error")
	}
}

func (e *ElasticCollector) SetFramesForTraces(ctx context.Context,
	req *SetFramesForTracesRequest) (*empty.Empty, error) {
	traces, err := CollectTracesAndFrames(req)
	if err != nil {
		counterFatalErr.Inc()
		e.logger.With(
			logp.Error(err),
			logp.String("grpc_method", "SetFramesForTraces"),
		).Error("error collecting frame metadata")
		return nil, errCustomer
	}
	counterStacktracesTotal.Add(int64(len(traces)))

	for _, trace := range traces {
		numTypes := len(trace.FrameTypes)
		numFiles := len(trace.Files)
		numLinenos := len(trace.Linenos)
		if numTypes != numFiles || numTypes != numLinenos {
			e.logger.With(
				logp.String("grpc_method", "SetFramesForTraces"),
			).Errorf("mismatch in number of data (%d) / linenos (%d) / types (%d)",
				numFiles, numLinenos, numTypes)
			continue
		}

		// Enqueue file IDs if Native or Kernel
		for i := 0; i < numFiles; i++ {
			if trace.FrameTypes[i].IsError() {
				continue
			}
			interpreterType, _ := trace.FrameTypes[i].Interpreter()
			if interpreterType == libpf.Native || interpreterType == libpf.Kernel {
				e.fileIDQueue.Add(trace.Files[i])
			}
		}

		if !trace.FrameTypes[0].IsError() {
			// Enqueue leaf frame if Native or Kernel
			interpreterType, _ := trace.FrameTypes[0].Interpreter()
			if interpreterType == libpf.Native || interpreterType == libpf.Kernel {
				e.leafFrameQueue.Add(common.MakeFrameID(trace.Files[0],
					uint64(trace.Linenos[0])))
			}
		}

		body, err := common.EncodeBodyBytes(StackTrace{
			FrameIDs: common.EncodeFrameIDs(trace.Files, trace.Linenos),
			Types:    common.EncodeFrameTypes(trace.FrameTypes),
		})
		if err != nil {
			e.logger.With(
				logp.Error(err),
				logp.String("grpc_method", "SetFramesForTraces"),
			).Error("failed to JSON encode stacktrace")
			return nil, errCustomer
		}

		// We use the base64-encoded trace hash as the document ID. This seems to be an
		// appropriate way to do K/V lookups with ES.
		docID := common.EncodeStackTraceID(trace.Hash)

		err = multiplexCurrentNextIndicesWrite(ctx, e, &esutil.BulkIndexerItem{
			Index:      common.StackTraceIndex,
			Action:     actionCreate,
			DocumentID: docID,
			OnFailure: func(
				_ context.Context,
				_ esutil.BulkIndexerItem,
				resp esutil.BulkIndexerResponseItem,
				_ error,
			) {
				if resp.Error.Type == docIDAlreadyExists {
					// Error is expected here, as we tried to "create" an existing document.
					// We increment the metric to understand the origin-to-duplicate ratio.
					counterStacktracesDuplicate.Inc()
					return
				}
				counterStacktracesFailure.Inc()
			},
		}, body)

		if err != nil {
			e.logger.With(
				logp.Error(err),
				logp.String("grpc_method", "SetFramesForTraces"),
			).Error("Elasticsearch indexing error")
			return nil, errCustomer
		}
	}

	return &emptypb.Empty{}, nil
}

func (e *ElasticCollector) AddFrameMetadata(ctx context.Context, in *AddFrameMetadataRequest) (
	*empty.Empty, error) {
	frames, err := CollectFrameMetadata(in)
	if err != nil {
		counterFatalErr.Inc()
		e.logger.With(
			logp.Error(err),
			logp.String("grpc_method", "AddFrameMetadata"),
		).Error("error collecting frame metadata")
		return nil, errCustomer
	}

	arraySize := len(frames)
	if arraySize == 0 {
		e.logger.With(
			logp.String("grpc_method", "AddFrameMetadata"),
		).Debug("request with no entries")
		return &empty.Empty{}, nil
	}
	counterStackframesTotal.Add(int64(arraySize))

	for _, frame := range frames {
		if frame.FileID.IsZero() {
			e.logger.With(
				logp.String("grpc_method", "AddFrameMetadata"),
			).Warn("attempting to report metadata for invalid FileID 0." +
				" This is likely a mistake and will be discarded.",
			)
			continue
		}

		e.sourceFilesLock.Lock()
		filename := frame.Filename
		if filename == "" {
			if v, ok := e.sourceFiles.Get(frame.SourceID); ok {
				filename = v.(string)
			}
		} else {
			e.sourceFiles.Add(frame.SourceID, filename)
		}
		e.sourceFilesLock.Unlock()

		body, err := common.EncodeBodyBytes(StackFrame{
			LineNumber:     int32(frame.LineNumber),
			FunctionName:   frame.FunctionName,
			FunctionOffset: int32(frame.FunctionOffset),
			FileName:       filename,
		})
		if err != nil {
			e.logger.With(
				logp.Error(err),
				logp.String("grpc_method", "AddFrameMetadata"),
			).Error("failed to JSON encode stackframe")
			return nil, errCustomer
		}

		docID := common.EncodeFrameID(frame.FileID, uint64(frame.AddressOrLine))
		err = multiplexCurrentNextIndicesWrite(ctx, e, &esutil.BulkIndexerItem{
			Index:      common.StackFrameIndex,
			Action:     actionCreate,
			DocumentID: docID,
			OnFailure: func(
				_ context.Context,
				_ esutil.BulkIndexerItem,
				resp esutil.BulkIndexerResponseItem,
				_ error,
			) {
				if resp.Error.Type == docIDAlreadyExists {
					// Error is expected here, as we tried to "create" an existing document.
					// We increment the metric to understand the origin-to-duplicate ratio.
					counterStackframesDuplicate.Inc()
					return
				}
				counterStackframesFailure.Inc()
			},
		}, body)

		if err != nil {
			e.logger.With(
				logp.Error(err),
				logp.String("grpc_method", "AddFrameMetadata"),
			).Error("Elasticsearch indexing error")
			return nil, errCustomer
		}
	}

	return &empty.Empty{}, nil
}

func (e *ElasticCollector) AddFallbackSymbols(ctx context.Context,
	in *AddFallbackSymbolsRequest) (*empty.Empty, error) {
	hiFileIDs := in.GetHiFileIDs()
	loFileIDs := in.GetLoFileIDs()
	symbols := in.GetSymbols()
	addressOrLines := in.GetAddressOrLines()

	arraySize := len(hiFileIDs)
	if arraySize == 0 {
		e.logger.With(
			logp.String("grpc_method", "AddFallbackSymbols"),
		).Debug("request with no entries")
		return &empty.Empty{}, nil
	}

	// Sanity check. Should never happen unless the HA is broken or client is malicious.
	if arraySize != len(loFileIDs) ||
		arraySize != len(addressOrLines) ||
		arraySize != len(symbols) {
		e.logger.With(
			logp.String("grpc_method", "AddFallbackSymbols"),
		).Errorf("mismatch in array sizes (%d)", arraySize)
		counterFatalErr.Inc()
		return nil, errCustomer
	}
	counterStackframesTotal.Add(int64(arraySize))

	for i := 0; i < arraySize; i++ {
		fileID := libpf.NewFileID(hiFileIDs[i], loFileIDs[i])

		if fileID.IsZero() {
			e.logger.With(
				logp.String("grpc_method", "AddFallbackSymbols"),
			).Warn("attempting to report metadata for invalid FileID 0." +
				" This is likely a mistake and will be discarded.")
			continue
		}

		body, err := common.EncodeBodyBytes(StackFrame{
			FunctionName: symbols[i],
		})
		if err != nil {
			e.logger.With(
				logp.Error(err),
				logp.String("grpc_method", "AddFallbackSymbols"),
			).Error("failed to JSON encode fallback stackframe")
			return nil, errCustomer
		}

		docID := common.EncodeFrameID(fileID, addressOrLines[i])

		err = multiplexCurrentNextIndicesWrite(ctx, e, &esutil.BulkIndexerItem{
			Index: common.StackFrameIndex,
			// Use 'create' instead of 'index' to not overwrite an existing document,
			// possibly containing a fully symbolized frame.
			Action:     actionCreate,
			DocumentID: docID,
			OnFailure: func(
				_ context.Context,
				_ esutil.BulkIndexerItem,
				resp esutil.BulkIndexerResponseItem,
				err error,
			) {
				if resp.Error.Type == docIDAlreadyExists {
					// Error is expected here, as we tried to "create" an existing document.
					// We increment the metric to understand the origin-to-duplicate ratio.
					counterStackframesDuplicate.Inc()
					return
				}
				counterStackframesFailure.Inc()
			},
		}, body)
		if err != nil {
			e.logger.With(
				logp.Error(err),
				logp.String("grpc_method", "AddFallbackSymbols"),
			).Error("Elasticsearch indexing error")
			return nil, errCustomer
		}
	}

	return &empty.Empty{}, nil
}

//go:embed metrics.json
var metricsDefFS embed.FS

type metricDef struct {
	Description string `json:"description"`
	MetricType  string `json:"type"`
	Name        string `json:"name"`
	FieldName   string `json:"field"`
	ID          uint32 `json:"id"`
	Obsolete    bool   `json:"obsolete"`
}

var fieldNames []string
var metricTypes []string

func init() {
	input, err := metricsDefFS.ReadFile("metrics.json")
	if err != nil {
		log.Fatalf("Failed to read from embedded metrics.json: %v", err)
	}

	var metricDefs []metricDef
	if err = json.Unmarshal(input, &metricDefs); err != nil {
		log.Fatalf("Failed to unmarshal from embedded metrics.json: %v", err)
	}

	maxID := uint32(0)
	for _, m := range metricDefs {
		// Plausibility check, we don't expect having that many metrics.
		if m.ID > 1000 {
			log.Fatalf("Unexpected high metric ID %d (needs manual adjustment)", m.ID)
		}
		if m.ID > maxID {
			maxID = m.ID
		}
	}

	fieldNames = make([]string, maxID+1)
	metricTypes = make([]string, maxID+1)

	for _, m := range metricDefs {
		if m.Obsolete {
			continue
		}
		fieldNames[m.ID] = m.FieldName
		metricTypes[m.ID] = m.MetricType
	}
}

func (e *ElasticCollector) AddMetrics(ctx context.Context, in *Metrics) (*empty.Empty, error) {
	tsmetrics := in.GetTsMetrics()
	ProjectID := GetProjectID(ctx)
	HostID := GetHostID(ctx)

	makeBody := func(metric *TsMetric) *bytes.Reader {
		var body bytes.Buffer

		body.WriteString(fmt.Sprintf(
			"{\"project.id\":%d,\"host.id\":%d,\"@timestamp\":%d,"+
				"\"ecs.version\":%q",
			ProjectID, HostID, metric.Timestamp, common.EcsVersionString))
		if e.clusterID != "" {
			body.WriteString(fmt.Sprintf(",\"Elasticsearch.cluster.id\":%q", e.clusterID))
		}
		for i, metricID := range metric.IDs {
			if int(metricID) >= len(metricTypes) {
				// Protect against panic on HA / collector version mismatch.
				// Do not log as this may happen often.
				continue
			}
			metricValue := metric.Values[i]
			metricType := metricTypes[metricID]
			fieldName := fieldNames[metricID]

			if metricValue == 0 && metricType == "counter" {
				// HA accidentally sends 0 counter values. Here we ignore them.
				// This check can be removed once the issue is fixed in the host agent.
				continue
			}

			if fieldName == "" {
				continue
			}

			body.WriteString(
				fmt.Sprintf(",%q:%d", fieldName, metricValue))
		}

		body.WriteString("}")
		return bytes.NewReader(body.Bytes())
	}

	for _, metric := range tsmetrics {
		if len(metric.IDs) != len(metric.Values) {
			e.logger.With(
				logp.String("grpc_method", "AddMetrics"),
			).Errorf("ignoring inconsistent metrics (ids: %d != values: %d)",
				len(metric.IDs), len(metric.Values))
			continue
		}
		err := e.metricsIndexer.Add(ctx, esutil.BulkIndexerItem{
			Index:  common.MetricsIndex,
			Action: actionCreate,
			Body:   makeBody(metric),
			OnFailure: func(
				_ context.Context,
				_ esutil.BulkIndexerItem,
				resp esutil.BulkIndexerResponseItem,
				err error,
			) {
				e.logger.With(
					logp.Error(err),
					logp.String("error_type", resp.Error.Type),
					logp.String("grpc_method", "AddMetrics"),
				).Error("failed to index host metrics")
			},
		})
		if err != nil {
			e.logger.With(
				logp.Error(err),
				logp.String("grpc_method", "AddMetrics"),
			).Error("Elasticsearch indexing error")
			return nil, errCustomer
		}
	}

	return &empty.Empty{}, nil
}

// multiplexCurrentNextIndicesWrite ingests twice the same item for 2 separate indices
// to achieve a sliding window ingestion mechanism.
// These indices will be managed by the custom ILM strategy implemented in ilm.go.
func multiplexCurrentNextIndicesWrite(ctx context.Context, e *ElasticCollector,
	item *esutil.BulkIndexerItem, body []byte) error {
	copied := *item
	copied.Index = nextIndex(item.Index)

	item.Body = bytes.NewReader(body)
	copied.Body = bytes.NewReader(body)

	if err := e.indexer.Add(ctx, *item); err != nil {
		return err
	}
	return e.indexer.Add(ctx, copied)
}
