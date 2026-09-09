package promptfilter

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

type Protocol string

const (
	ProtocolUnknown   Protocol = "unknown"
	ProtocolResponses Protocol = "responses"
	ProtocolChat      Protocol = "chat_completions"
	ProtocolMessages  Protocol = "messages"
	ProtocolImages    Protocol = "images"
	ProtocolSearch    Protocol = "alpha_search"
)

type Transport string

const (
	TransportHTTP      Transport = "http"
	TransportWebSocket Transport = "websocket"
)

type ModelFamily string

const (
	ModelFamilyOpenAI    ModelFamily = "openai"
	ModelFamilyAnthropic ModelFamily = "anthropic"
	ModelFamilyXAI       ModelFamily = "xai"
	ModelFamilyUnknown   ModelFamily = "unknown"
)

type SegmentOrigin string

type SegmentTrust string

const (
	OriginCurrentUser          SegmentOrigin = "current_user"
	OriginHistory              SegmentOrigin = "history"
	OriginSystem               SegmentOrigin = "system"
	OriginDeveloper            SegmentOrigin = "developer"
	OriginInstructions         SegmentOrigin = "instructions"
	OriginToolOutput           SegmentOrigin = "tool_output"
	OriginToolArguments        SegmentOrigin = "tool_arguments"
	OriginAttachmentRefs       SegmentOrigin = "attachment_refs"
	OriginSessionContext       SegmentOrigin = "session_context"
	OriginApplicationCandidate SegmentOrigin = "application_candidate"
	OriginAttachmentContent    SegmentOrigin = "attachment_content"
)

const (
	SegmentTrustClientSupplied SegmentTrust = "client_supplied"
	SegmentTrustGatewaySigned  SegmentTrust = "gateway_signed"
	SegmentTrustServerInjected SegmentTrust = "server_injected"
)

// Segment keeps request text and its provenance separate. Sequence reflects
// wire order within the request; Role retains the protocol role when present.
type Segment struct {
	Origin         SegmentOrigin `json:"origin"`
	Role           string        `json:"role,omitempty"`
	Text           string        `json:"text"`
	Sequence       int           `json:"sequence"`
	Linked         bool          `json:"linked,omitempty"`
	Truncated      bool          `json:"truncated,omitempty"`
	Trust          SegmentTrust  `json:"trust"`
	SafetyEvidence string        `json:"-"`
	SafetyPriority int           `json:"-"`
}

type RequestEnvelope struct {
	Endpoint            string      `json:"endpoint"`
	Protocol            Protocol    `json:"protocol"`
	Transport           Transport   `json:"transport"`
	RequestedModel      string      `json:"requested_model,omitempty"`
	EffectiveModel      string      `json:"effective_model,omitempty"`
	ModelFamily         ModelFamily `json:"model_family"`
	Segments            []Segment   `json:"segments"`
	AdapterUnclassified bool        `json:"adapter_unclassified,omitempty"`
	// AdapterUnclassifiedTypes records the offending typed-payload names (bounded,
	// deduped) so the non-punitive adapter audit can surface which future block or
	// item type went unrecognized, instead of only flagging that one did.
	AdapterUnclassifiedTypes []string `json:"adapter_unclassified_types,omitempty"`
	Truncated                bool     `json:"truncated,omitempty"`
	CurrentUserTruncated     bool     `json:"current_user_truncated,omitempty"`
	AuxiliaryTruncated       bool     `json:"auxiliary_truncated,omitempty"`
	currentUserExactText     string
	currentUserPrecheck      *currentUserPrecheck
	precheckIncomplete       bool
	approvalSurfaceOK        bool
}

func BuildEnvelope(body []byte, endpoint string, requestedModel string, transport Transport, maxLen int) RequestEnvelope {
	return BuildEnvelopeWithModels(body, endpoint, requestedModel, "", transport, maxLen)
}

func BuildEnvelopeWithModels(body []byte, endpoint string, requestedModel string, effectiveModel string, transport Transport, maxLen int) RequestEnvelope {
	if maxLen <= 0 {
		maxLen = DefaultMaxTextLength
	}
	return buildEnvelopeWithBudget(body, endpoint, requestedModel, effectiveModel, transport, envelopeBudget{
		maxSegments:      MaxGuardMaxSegments,
		currentUserBytes: maxLen,
		auxiliaryBytes:   maxLen,
		scanChunkBytes:   DefaultGuardScanChunkBytes,
		scanOverlapBytes: DefaultGuardScanOverlapBytes,
		priorityScanner:  builtinDecodedSafetyPriorityScanner(),
	})
}

// BuildEnvelopeWithModelsAndPerformance applies the normalized Guard budget at
// extraction time. legacyMaxLen remains the compatibility fallback for callers
// loading an older configuration that predates source-specific budgets.
func BuildEnvelopeWithModelsAndPerformance(body []byte, endpoint string, requestedModel string, effectiveModel string, transport Transport, legacyMaxLen int, performance GuardPerformanceConfig) RequestEnvelope {
	performance = normalizeEnvelopePerformance(performance, legacyMaxLen)
	return buildEnvelopeWithBudget(body, endpoint, requestedModel, effectiveModel, transport, envelopeBudget{
		maxSegments:      performance.MaxSegments,
		currentUserBytes: performance.MaxCurrentUserBytes,
		auxiliaryBytes:   performance.MaxAuxiliaryBytes,
		scanChunkBytes:   performance.ScanChunkBytes,
		scanOverlapBytes: performance.ScanOverlapBytes,
		priorityScanner:  builtinDecodedSafetyPriorityScanner(),
	})
}

// BuildEnvelopeWithModelsAndConfig applies the active normalized rule set to
// overflow evidence discovery before destructive sampling. This keeps custom
// strict rules and administrator pattern updates equivalent to built-ins when
// their match lies outside the synchronous head/tail byte budget.
func BuildEnvelopeWithModelsAndConfig(body []byte, endpoint string, requestedModel string, effectiveModel string, transport Transport, cfg Config) RequestEnvelope {
	cfg = NormalizeConfig(cfg)
	performance := normalizeEnvelopePerformance(cfg.Advanced.Guard.Performance, cfg.MaxTextLength)
	engine, err := engineForConfig(cfg)
	priorityScanner := builtinDecodedSafetyPriorityScanner()
	if err == nil && engine != nil {
		priorityScanner = decodedSafetyPriorityScannerForEngine(engine)
	}
	return buildEnvelopeWithBudget(body, endpoint, requestedModel, effectiveModel, transport, envelopeBudget{
		maxSegments:      performance.MaxSegments,
		currentUserBytes: performance.MaxCurrentUserBytes,
		auxiliaryBytes:   performance.MaxAuxiliaryBytes,
		scanChunkBytes:   performance.ScanChunkBytes,
		scanOverlapBytes: performance.ScanOverlapBytes,
		priorityScanner:  priorityScanner,
		exactEngine:      engine,
	})
}

// BuildTextEnvelopeWithModelsAndConfig builds the same bounded, exact-aware
// current-user envelope used by JSON protocol adapters. It is intended for
// transports such as multipart image edits where the prompt has already been
// parsed separately from binary attachment data.
func BuildTextEnvelopeWithModelsAndConfig(text string, endpoint string, requestedModel string, effectiveModel string, transport Transport, cfg Config) RequestEnvelope {
	cfg = NormalizeConfig(cfg)
	performance := normalizeEnvelopePerformance(cfg.Advanced.Guard.Performance, cfg.MaxTextLength)
	engine, err := engineForConfig(cfg)
	priorityScanner := builtinDecodedSafetyPriorityScanner()
	if err == nil && engine != nil {
		priorityScanner = decodedSafetyPriorityScannerForEngine(engine)
	}
	if transport != TransportWebSocket {
		transport = TransportHTTP
	}
	requestedModel = strings.TrimSpace(requestedModel)
	effectiveModel = strings.TrimSpace(effectiveModel)
	envelope := RequestEnvelope{
		Endpoint:       strings.TrimSpace(endpoint),
		Protocol:       ProtocolForEndpoint(endpoint),
		Transport:      transport,
		RequestedModel: requestedModel,
		EffectiveModel: effectiveModel,
		ModelFamily:    ResolveModelFamily(requestedModel, effectiveModel),
	}
	if envelope.Protocol == ProtocolUnknown {
		envelope.AdapterUnclassified = true
		return envelope
	}
	builder := envelopeBuilder{
		envelope:         &envelope,
		maxSegments:      performance.MaxSegments,
		currentUserBytes: performance.MaxCurrentUserBytes,
		auxiliaryBytes:   performance.MaxAuxiliaryBytes,
		scanChunkBytes:   performance.ScanChunkBytes,
		scanOverlapBytes: performance.ScanOverlapBytes,
		priorityScanner:  priorityScanner,
		exactEngine:      engine,
		exactCapture:     engine != nil,
		exactSourceOK:    engine != nil,
	}
	builder.append(OriginCurrentUser, "user", text)
	builder.finalize()
	return envelope
}

func decodedSafetyPriorityScannerForConfig(cfg Config) decodedSafetyPriorityScanner {
	if engine, err := engineForConfig(cfg); err == nil && engine != nil {
		return decodedSafetyPriorityScannerForEngine(engine)
	}
	return builtinDecodedSafetyPriorityScanner()
}

type envelopeBudget struct {
	maxSegments      int
	currentUserBytes int
	auxiliaryBytes   int
	scanChunkBytes   int
	scanOverlapBytes int
	priorityScanner  decodedSafetyPriorityScanner
	exactEngine      *Engine
}

func buildEnvelopeWithBudget(body []byte, endpoint string, requestedModel string, effectiveModel string, transport Transport, budget envelopeBudget) RequestEnvelope {
	protocol := ProtocolForEndpoint(endpoint)
	if transport != TransportWebSocket {
		transport = TransportHTTP
	}
	if requestedModel = strings.TrimSpace(requestedModel); requestedModel == "" && gjson.ValidBytes(body) {
		requestedModel = strings.TrimSpace(gjson.GetBytes(body, "model").String())
	}
	envelope := RequestEnvelope{
		Endpoint:       strings.TrimSpace(endpoint),
		Protocol:       protocol,
		Transport:      transport,
		RequestedModel: requestedModel,
		EffectiveModel: strings.TrimSpace(effectiveModel),
		ModelFamily:    ResolveModelFamily(requestedModel, effectiveModel),
	}
	if protocol == ProtocolUnknown {
		envelope.AdapterUnclassified = true
		return envelope
	}
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return envelope
	}

	builder := envelopeBuilder{
		envelope:         &envelope,
		maxSegments:      budget.maxSegments,
		currentUserBytes: budget.currentUserBytes,
		auxiliaryBytes:   budget.auxiliaryBytes,
		scanChunkBytes:   budget.scanChunkBytes,
		scanOverlapBytes: budget.scanOverlapBytes,
		priorityScanner:  budget.priorityScanner,
		exactEngine:      budget.exactEngine,
		exactCapture:     budget.exactEngine != nil,
		exactSourceOK:    budget.exactEngine != nil,
	}
	switch protocol {
	case ProtocolResponses:
		builder.appendResult(OriginInstructions, "developer", gjson.GetBytes(body, "instructions"))
		builder.appendMessages(gjson.GetBytes(body, "input"))
		builder.appendResult(OriginCurrentUser, "user", gjson.GetBytes(body, "prompt"))
		if !envelopeHasOrigin(envelope, OriginCurrentUser) {
			builder.appendMessages(gjson.GetBytes(body, "messages"))
		}
	case ProtocolChat:
		builder.appendResult(OriginInstructions, "developer", gjson.GetBytes(body, "instructions"))
		builder.appendMessages(gjson.GetBytes(body, "messages"))
	case ProtocolMessages:
		builder.appendResult(OriginSystem, "system", gjson.GetBytes(body, "system"))
		builder.appendMessages(gjson.GetBytes(body, "messages"))
	case ProtocolImages:
		builder.appendResult(OriginCurrentUser, "user", gjson.GetBytes(body, "prompt"))
		builder.appendResult(OriginCurrentUser, "user", gjson.GetBytes(body, "style"))
		builder.appendAttachmentReferences(gjson.ParseBytes(body), "user")
	case ProtocolSearch:
		builder.appendResult(OriginCurrentUser, "user", gjson.GetBytes(body, "commands.search_query.#.q"))
	}
	builder.finalize()
	if text, ok := closedApprovalReassessmentSurface(body, requestedModel); ok {
		envelope.approvalSurfaceOK = true
		collapseApprovalCurrentUser(&envelope, text)
	}
	return envelope
}

func ProtocolForEndpoint(endpoint string) Protocol {
	switch strings.ToLower(strings.TrimSpace(endpoint)) {
	case "response", "responses", "responses_compact", "realtime", "/responses", "/realtime", "/v1/responses", "/v1/responses/compact", "/v1/realtime":
		return ProtocolResponses
	case "chat", "chat_completions", "/v1/chat/completions":
		return ProtocolChat
	case "messages", "anthropic", "/v1/messages":
		return ProtocolMessages
	case "image", "images", "images_generations", "images_edits", "images_jobs", "/v1/images/generations", "/v1/images/edits", "/v1/images/jobs", "/v1/images/jobs:edit":
		return ProtocolImages
	case "alpha_search", "/v1/alpha/search", "/backend-api/codex/alpha/search":
		return ProtocolSearch
	default:
		return ProtocolUnknown
	}
}

func ResolveModelFamily(requestedModel string, effectiveModel string) ModelFamily {
	model := strings.ToLower(strings.TrimSpace(effectiveModel))
	if model == "" {
		model = strings.ToLower(strings.TrimSpace(requestedModel))
	}
	switch {
	case strings.HasPrefix(model, "claude") || strings.Contains(model, "anthropic"):
		return ModelFamilyAnthropic
	case strings.HasPrefix(model, "grok") || strings.Contains(model, "xai") || strings.Contains(model, "x-ai"):
		return ModelFamilyXAI
	case strings.HasPrefix(model, "gpt-") || strings.HasPrefix(model, "chatgpt-") || strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4") || strings.Contains(model, "codex"):
		return ModelFamilyOpenAI
	default:
		return ModelFamilyUnknown
	}
}

func (e RequestEnvelope) SegmentsForOrigin(origin SegmentOrigin) []Segment {
	out := make([]Segment, 0)
	for _, segment := range e.Segments {
		if segment.Origin == origin {
			out = append(out, segment)
		}
	}
	return out
}

func envelopeHasOrigin(envelope RequestEnvelope, origin SegmentOrigin) bool {
	for _, segment := range envelope.Segments {
		if segment.Origin == origin {
			return true
		}
	}
	return false
}

func normalizeEnvelopePerformance(performance GuardPerformanceConfig, legacyMaxLen int) GuardPerformanceConfig {
	if legacyMaxLen <= 0 {
		legacyMaxLen = DefaultMaxTextLength
	}
	allBudgetFieldsMissing := performance.MaxSegments == 0 &&
		performance.MaxCurrentUserBytes == 0 &&
		performance.MaxAuxiliaryBytes == 0 &&
		performance.ScanChunkBytes == 0 &&
		performance.ScanOverlapBytes == 0
	if performance.MaxSegments <= 0 {
		performance.MaxSegments = MaxGuardMaxSegments
	}
	if performance.MaxSegments > MaxGuardMaxSegments {
		performance.MaxSegments = MaxGuardMaxSegments
	}
	if performance.MaxCurrentUserBytes <= 0 {
		performance.MaxCurrentUserBytes = legacyMaxLen
	}
	if performance.MaxCurrentUserBytes > MaxGuardCurrentUserBytes {
		performance.MaxCurrentUserBytes = MaxGuardCurrentUserBytes
	}
	if allBudgetFieldsMissing {
		performance.MaxAuxiliaryBytes = legacyMaxLen
	} else if performance.MaxAuxiliaryBytes < 0 {
		performance.MaxAuxiliaryBytes = legacyMaxLen
	}
	if performance.MaxAuxiliaryBytes > MaxGuardAuxiliaryBytes {
		performance.MaxAuxiliaryBytes = MaxGuardAuxiliaryBytes
	}
	return performance
}

// ApplyGuardPerformanceBudget bounds all request sources after adapters and
// optional extensions have contributed their segments. Current-user input and
// linked continuation context take precedence over auxiliary sources, while
// the final slice remains in wire order. Exceeding a budget is metadata only:
// it never creates a policy signal or blocking decision by itself.
func ApplyGuardPerformanceBudget(envelope RequestEnvelope, performance GuardPerformanceConfig, legacyMaxLen int) RequestEnvelope {
	return applyGuardPerformanceBudgetWithScanner(envelope, performance, legacyMaxLen, builtinDecodedSafetyPriorityScanner())
}

func applyGuardPerformanceBudgetWithScanner(envelope RequestEnvelope, performance GuardPerformanceConfig, legacyMaxLen int, priorityScanner decodedSafetyPriorityScanner) RequestEnvelope {
	performance = normalizeEnvelopePerformance(performance, legacyMaxLen)
	segments := append([]Segment(nil), envelope.Segments...)
	sort.SliceStable(segments, func(i, j int) bool {
		return segments[i].Sequence < segments[j].Sequence
	})

	currentSegments := make([]Segment, 0, len(segments))
	auxiliarySegments := make([]Segment, 0, len(segments))
	for _, segment := range segments {
		if segmentUsesCurrentUserBudget(segment) {
			currentSegments = append(currentSegments, segment)
		} else {
			auxiliarySegments = append(auxiliarySegments, segment)
		}
	}
	selectedCurrent, currentTruncated := sampleSegmentClass(
		currentSegments,
		performance.MaxSegments,
		performance.MaxCurrentUserBytes,
		performance.ScanChunkBytes,
		performance.ScanOverlapBytes,
		true,
		priorityScanner,
	)
	remainingSegments := performance.MaxSegments - len(selectedCurrent)
	selectedAuxiliary, auxiliaryTruncated := sampleSegmentClass(
		auxiliarySegments,
		remainingSegments,
		performance.MaxAuxiliaryBytes,
		performance.ScanChunkBytes,
		performance.ScanOverlapBytes,
		false,
		priorityScanner,
	)
	selected := append(selectedCurrent, selectedAuxiliary...)
	if currentTruncated {
		envelope.Truncated = true
		envelope.CurrentUserTruncated = true
	}
	if auxiliaryTruncated {
		envelope.Truncated = true
		envelope.AuxiliaryTruncated = true
	}
	sort.SliceStable(selected, func(i, j int) bool {
		return selected[i].Sequence < selected[j].Sequence
	})
	envelope.Segments = selected
	return envelope
}

type segmentSampleAllocation struct {
	evidence int
	head     int
	tail     int
}

// sampleSegmentClass retains a bounded, provenance-preserving view of one
// source class. When the class is over budget, high-confidence evidence found
// by the lightweight window scanner is reserved first, then the remaining
// bytes and slots are split between the beginning and end. Length alone never
// creates a policy signal, but an attacker cannot hide a built-in terminal
// phrase merely by placing it after a large benign prefix or in a later part.
func sampleSegmentClass(segments []Segment, maxSegments, maxBytes, chunkBytes, overlapBytes int, scanPriority bool, priorityScanner decodedSafetyPriorityScanner) ([]Segment, bool) {
	if len(segments) == 0 {
		return nil, false
	}
	totalBytes := 0
	truncated := false
	for _, segment := range segments {
		totalBytes += len(segment.Text)
		truncated = truncated || segment.Truncated
	}
	if maxSegments <= 0 || maxBytes <= 0 {
		return nil, true
	}
	if !truncated && len(segments) <= maxSegments && totalBytes <= maxBytes {
		return append([]Segment(nil), segments...), false
	}
	truncated = true
	working := append([]Segment(nil), segments...)
	if scanPriority {
		for index := range working {
			if working[index].SafetyEvidence != "" {
				continue
			}
			working[index].SafetyPriority, working[index].SafetyEvidence = scanPlainTextPriorityWithWindow(working[index].Text, chunkBytes, overlapBytes, priorityScanner)
		}
	}

	allocations := make(map[int]segmentSampleAllocation, min(len(working), maxSegments))
	remainingBytes := maxBytes
	remainingSlots := maxSegments
	evidenceIndexes := make([]int, 0, len(working))
	for index := range working {
		if strings.TrimSpace(working[index].SafetyEvidence) != "" {
			evidenceIndexes = append(evidenceIndexes, index)
		}
	}
	sort.SliceStable(evidenceIndexes, func(i, j int) bool {
		left, right := working[evidenceIndexes[i]], working[evidenceIndexes[j]]
		if left.SafetyPriority == right.SafetyPriority {
			return left.Sequence < right.Sequence
		}
		return left.SafetyPriority > right.SafetyPriority
	})
	for _, index := range evidenceIndexes {
		evidence := strings.TrimSpace(working[index].SafetyEvidence)
		if evidence == "" || remainingBytes <= 0 || remainingSlots <= 0 {
			continue
		}
		allocated := min(len(evidence), remainingBytes)
		if allocated <= 0 {
			continue
		}
		allocations[index] = segmentSampleAllocation{evidence: allocated}
		remainingBytes -= allocated
		remainingSlots--
	}

	if remainingBytes > 0 && remainingSlots > 0 {
		headBytes := remainingBytes * 4 / 5
		tailBytes := remainingBytes - headBytes
		headSlots := remainingSlots * 4 / 5
		if headSlots == 0 {
			headSlots = 1
		}
		if headSlots > remainingSlots {
			headSlots = remainingSlots
		}
		tailSlots := remainingSlots - headSlots
		if remainingSlots > 1 && tailSlots == 0 {
			tailSlots = 1
			headSlots--
		}

		for index := 0; index < len(working) && headBytes > 0 && headSlots > 0; index++ {
			if _, exists := allocations[index]; exists {
				continue
			}
			allocated := min(len(working[index].Text), headBytes)
			if allocated <= 0 {
				continue
			}
			allocations[index] = segmentSampleAllocation{head: allocated}
			headBytes -= allocated
			headSlots--
		}
		for index := len(working) - 1; index >= 0 && tailBytes > 0 && tailSlots > 0; index-- {
			if _, exists := allocations[index]; exists {
				continue
			}
			allocated := min(len(working[index].Text), tailBytes)
			if allocated <= 0 {
				continue
			}
			allocations[index] = segmentSampleAllocation{tail: allocated}
			tailBytes -= allocated
			tailSlots--
		}
	}

	selected := make([]Segment, 0, len(allocations))
	for index, original := range working {
		allocation, ok := allocations[index]
		if !ok {
			continue
		}
		segment := original
		switch {
		case allocation.evidence > 0:
			segment.Text = safeUTF8Prefix(strings.TrimSpace(segment.SafetyEvidence), allocation.evidence)
		case allocation.head > 0 && allocation.tail > 0:
			segment.Text = limitScanTextExact(segment.Text, allocation.head+allocation.tail)
		case allocation.head > 0:
			segment.Text = safeUTF8Prefix(segment.Text, allocation.head)
		case allocation.tail > 0:
			segment.Text = safeUTF8Suffix(segment.Text, allocation.tail)
		}
		segment.Text = strings.TrimSpace(segment.Text)
		if segment.Text == "" {
			continue
		}
		segment.Truncated = segment.Truncated || len(segment.Text) < len(original.Text)
		selected = append(selected, segment)
	}
	return selected, truncated
}

func scanPlainTextPriorityWithWindow(text string, chunkBytes, overlapBytes int, scanner decodedSafetyPriorityScanner) (int, string) {
	if text == "" {
		return 0, ""
	}
	if chunkBytes <= 0 {
		chunkBytes = DefaultGuardScanChunkBytes
	}
	// This pass only locates high-confidence evidence in text that would
	// otherwise be discarded. Larger windows amortize normalization and regex
	// setup while the configured overlap still protects cross-window phrases.
	if chunkBytes < MaxGuardScanChunkBytes {
		chunkBytes = MaxGuardScanChunkBytes
	}
	if overlapBytes <= 0 || overlapBytes >= chunkBytes {
		overlapBytes = DefaultGuardScanOverlapBytes
		if overlapBytes >= chunkBytes {
			overlapBytes = max(1, chunkBytes/16)
		}
	}
	priority := 0
	evidence := ""
	for start := 0; start < len(text); {
		end := min(start+chunkBytes, len(text))
		for end > start && end < len(text) && !utf8.RuneStart(text[end]) {
			end--
		}
		if end <= start {
			_, size := utf8.DecodeRuneInString(text[start:])
			if size <= 0 {
				break
			}
			end = min(start+size, len(text))
		}
		windowStart := max(0, start-overlapBytes)
		for windowStart < start && !utf8.RuneStart(text[windowStart]) {
			windowStart++
		}
		source := text[windowStart:end]
		matchedHints, unhinted := decodedSafetyPriorityMatchedHintsSource(source, scanner)
		if !unhinted && !decodedSafetyPriorityMatchedHintSetCanMatch(scanner, matchedHints) {
			start = end
			continue
		}
		normalized := normalizeForScan(source)
		candidatePriority, candidateEvidence := decodedSafetyPriorityNormalizedWithHints(normalized, scanner, matchedHints)
		if candidatePriority > priority || candidatePriority == priority && evidence == "" && candidateEvidence != "" {
			priority = candidatePriority
			evidence = candidateEvidence
		}
		start = end
	}
	return priority, evidence
}

func segmentUsesCurrentUserBudget(segment Segment) bool {
	return segment.Origin == OriginCurrentUser ||
		segment.Origin == OriginApplicationCandidate ||
		(segment.Origin == OriginHistory && segment.Linked)
}

type envelopeBuilder struct {
	envelope               *RequestEnvelope
	maxSegments            int
	currentUserBytes       int
	auxiliaryBytes         int
	scanChunkBytes         int
	scanOverlapBytes       int
	priorityScanner        decodedSafetyPriorityScanner
	exactEngine            *Engine
	exactCapture           bool
	exactSourceParts       []string
	exactSourceBytes       int
	exactSourceOK          bool
	linkedCandidateCapture bool
	linkedCandidateParts   []string
	linkedCandidateBytes   int
	linkedCandidateOK      bool
	currentSegments        int
	auxiliarySegments      int
	sequence               int
}

func (b *envelopeBuilder) append(origin SegmentOrigin, role string, text string) {
	if b == nil || b.envelope == nil {
		return
	}
	segmentCount := &b.auxiliarySegments
	if origin == OriginCurrentUser {
		segmentCount = &b.currentSegments
	}
	originalBytes := len(text)
	if origin == OriginCurrentUser {
		b.recordExactCurrentUser(text)
	}
	if origin == OriginHistory && b.linkedCandidateCapture {
		b.recordLinkedCandidate(text)
	}
	fieldLimit := b.auxiliaryBytes
	if origin == OriginCurrentUser || origin == OriginHistory {
		fieldLimit = max(b.currentUserBytes, b.auxiliaryBytes)
	}
	if fieldLimit <= 0 {
		if originalBytes > 0 {
			b.markTruncated(origin)
		}
		return
	}
	segmentOverflow := b.maxSegments > 0 && *segmentCount >= b.maxSegments
	safetyPriority, safetyEvidence := 0, ""
	if origin == OriginCurrentUser && (originalBytes > fieldLimit || segmentOverflow) && !b.exactSourceOK {
		safetyPriority, safetyEvidence = scanPlainTextPriorityWithWindow(text, b.scanChunkBytes, b.scanOverlapBytes, b.priorityScanner)
	}
	text = limitScanTextExact(text, fieldLimit)
	text = strings.TrimSpace(text)
	if text == "" {
		if originalBytes > 0 && fieldLimit > 0 && originalBytes > fieldLimit {
			b.markTruncated(origin)
		}
		return
	}
	truncated := fieldLimit > 0 && originalBytes > fieldLimit
	segment := Segment{
		Origin: origin, Role: strings.ToLower(strings.TrimSpace(role)), Text: text, Sequence: b.sequence, Truncated: truncated, Trust: SegmentTrustClientSupplied,
		SafetyEvidence: safetyEvidence,
		SafetyPriority: safetyPriority,
	}
	if segmentOverflow {
		b.markTruncated(origin)
		if origin == OriginCurrentUser {
			b.mergeCurrentUserOverflow(segment, fieldLimit)
			b.sequence++
		}
		return
	}
	b.envelope.Segments = append(b.envelope.Segments, segment)
	*segmentCount++
	if truncated {
		b.markTruncated(origin)
	}
	b.sequence++
}

func (b *envelopeBuilder) mergeCurrentUserOverflow(segment Segment, fieldLimit int) {
	if b == nil || b.envelope == nil || fieldLimit <= 0 {
		return
	}
	for index := len(b.envelope.Segments) - 1; index >= 0; index-- {
		current := b.envelope.Segments[index]
		if current.Origin != OriginCurrentUser {
			continue
		}
		combined := current.Text + "\n" + segment.Text
		evidence := strings.TrimSpace(current.SafetyEvidence)
		priority := current.SafetyPriority
		if candidate := strings.TrimSpace(segment.SafetyEvidence); candidate != "" && (evidence == "" || segment.SafetyPriority > priority) {
			evidence = candidate
			priority = segment.SafetyPriority
		}
		current.Text = limitScanTextExact(combined, fieldLimit)
		current.Sequence = segment.Sequence
		current.Truncated = true
		current.SafetyEvidence = evidence
		current.SafetyPriority = priority
		b.envelope.Segments[index] = current
		return
	}
}

func (b *envelopeBuilder) markTruncated(origin SegmentOrigin) {
	if b == nil || b.envelope == nil {
		return
	}
	b.envelope.Truncated = true
	if origin == OriginCurrentUser {
		b.envelope.CurrentUserTruncated = true
		return
	}
	b.envelope.AuxiliaryTruncated = true
}

func (b *envelopeBuilder) recordExactCurrentUser(text string) {
	if b == nil || !b.exactSourceOK || strings.TrimSpace(text) == "" {
		return
	}
	separatorBytes := 0
	if len(b.exactSourceParts) > 0 {
		separatorBytes = 1
	}
	if len(text) > MaxGuardCurrentUserBytes-b.exactSourceBytes-separatorBytes {
		b.exactSourceParts = nil
		b.exactSourceBytes = 0
		b.exactSourceOK = false
		return
	}
	b.exactSourceParts = append(b.exactSourceParts, text)
	b.exactSourceBytes += separatorBytes + len(text)
}

func (b *envelopeBuilder) beginLinkedCandidateCapture() {
	if b == nil {
		return
	}
	b.linkedCandidateParts = nil
	b.linkedCandidateBytes = 0
	b.linkedCandidateCapture = b.exactCapture
	b.linkedCandidateOK = b.exactCapture
}

func (b *envelopeBuilder) recordLinkedCandidate(text string) {
	if b == nil || !b.linkedCandidateCapture || !b.linkedCandidateOK || strings.TrimSpace(text) == "" {
		return
	}
	separatorBytes := 0
	if len(b.linkedCandidateParts) > 0 {
		separatorBytes = 1
	}
	if len(text) > MaxGuardCurrentUserBytes-b.linkedCandidateBytes-separatorBytes {
		b.linkedCandidateParts = nil
		b.linkedCandidateBytes = 0
		b.linkedCandidateOK = false
		return
	}
	b.linkedCandidateParts = append(b.linkedCandidateParts, text)
	b.linkedCandidateBytes += separatorBytes + len(text)
}

func (b *envelopeBuilder) discardLinkedCandidate() {
	if b == nil {
		return
	}
	b.linkedCandidateCapture = false
	b.linkedCandidateParts = nil
	b.linkedCandidateBytes = 0
	b.linkedCandidateOK = false
}

func (b *envelopeBuilder) promoteLinkedCandidate() {
	if b == nil {
		return
	}
	defer b.discardLinkedCandidate()
	if !b.exactCapture {
		return
	}
	if !b.linkedCandidateOK || !b.exactSourceOK {
		b.exactSourceParts = nil
		b.exactSourceBytes = 0
		b.exactSourceOK = false
		return
	}
	if len(b.linkedCandidateParts) == 0 {
		return
	}
	separatorBytes := 0
	if len(b.exactSourceParts) > 0 {
		separatorBytes = 1
	}
	if b.linkedCandidateBytes > MaxGuardCurrentUserBytes-b.exactSourceBytes-separatorBytes {
		b.exactSourceParts = nil
		b.exactSourceBytes = 0
		b.exactSourceOK = false
		return
	}
	parts := make([]string, 0, len(b.linkedCandidateParts)+len(b.exactSourceParts))
	parts = append(parts, b.linkedCandidateParts...)
	parts = append(parts, b.exactSourceParts...)
	b.exactSourceParts = parts
	b.exactSourceBytes += separatorBytes + b.linkedCandidateBytes
}

func (b *envelopeBuilder) finalize() {
	if b == nil || b.envelope == nil {
		return
	}
	bounded := ApplyGuardPerformanceBudget(*b.envelope, GuardPerformanceConfig{
		MaxSegments:         b.maxSegments,
		MaxCurrentUserBytes: b.currentUserBytes,
		MaxAuxiliaryBytes:   b.auxiliaryBytes,
	}, max(b.currentUserBytes, b.auxiliaryBytes))
	if bounded.CurrentUserTruncated && b.exactSourceOK && len(b.exactSourceParts) > 0 {
		bounded.currentUserExactText = strings.TrimSpace(strings.Join(b.exactSourceParts, " "))
	}
	if bounded.CurrentUserTruncated && b.exactCapture && !b.exactSourceOK {
		bounded.precheckIncomplete = true
	}
	*b.envelope = bounded
}

func (b *envelopeBuilder) appendResult(origin SegmentOrigin, role string, result gjson.Result) {
	if !result.Exists() || result.Type == gjson.Null {
		return
	}
	switch {
	case result.IsArray():
		for _, item := range result.Array() {
			b.appendResult(origin, role, item)
		}
	case result.IsObject():
		blockType := strings.ToLower(strings.TrimSpace(result.Get("type").String()))
		switch blockType {
		case "tool_result", "function_call_output", "computer_call_output", "mcp_call_output":
			b.appendResult(OriginToolOutput, "tool", firstExistingResult(result, "output", "content", "text"))
		case "tool_use", "function_call", "computer_call", "mcp_call":
			b.appendToolArguments(result, role)
		case "encrypted_content":
			// Opaque server-side encrypted continuation payload (gpt-5.6 store=false
			// reasoning). It carries no scannable plaintext, so it is recognized and
			// skipped rather than flagged as an unclassified block.
			return
		default:
			if blockType != "" && !recognizedEnvelopeContentBlockType(blockType) {
				// A future typed block has no proven provenance contract. Do not
				// reinterpret its generic text/content fields as the caller-provided
				// origin; record an adapter audit marker and fail open for this block.
				b.markAdapterUnclassified(blockType)
				return
			}
			if text := result.Get("text"); text.Type == gjson.String {
				b.append(origin, role, text.String())
			}
			if content := result.Get("content"); content.Exists() {
				b.appendResult(origin, role, content)
			}
		}
		b.appendAttachmentReferences(result, role)
	case result.Type == gjson.String:
		b.append(origin, role, result.String())
	}
}

func (b *envelopeBuilder) appendMessages(result gjson.Result) {
	if !result.Exists() || result.Type == gjson.Null {
		return
	}
	if !result.IsArray() {
		if result.IsObject() {
			if role := strings.ToLower(strings.TrimSpace(result.Get("role").String())); role != "" {
				b.appendMessage(result, role, true)
				return
			}
			if itemType := strings.ToLower(strings.TrimSpace(result.Get("type").String())); itemType != "" {
				b.appendTypedInputItem(result, true)
				return
			}
		}
		b.appendResult(OriginCurrentUser, "user", result)
		return
	}

	items := result.Array()
	currentItems := selectCurrentInputItems(items)
	firstCurrent := -1
	for index, selected := range currentItems {
		if selected {
			firstCurrent = index
			break
		}
	}
	previousUser := previousUserCandidate(items, firstCurrent)
	segmentStarts := make([]int, len(items))
	segmentEnds := make([]int, len(items))
	for index, item := range items {
		if index == previousUser {
			b.beginLinkedCandidateCapture()
		}
		segmentStarts[index] = len(b.envelope.Segments)
		if role := inputItemRole(item); role != "" {
			b.appendMessage(item, role, currentItems[index])
		} else {
			b.appendTypedInputItem(item, currentItems[index])
		}
		segmentEnds[index] = len(b.envelope.Segments)
		if index == previousUser {
			b.linkedCandidateCapture = false
		}
	}

	currentText := make([]string, 0, 2)
	for index, selected := range currentItems {
		if !selected {
			continue
		}
		for _, segment := range b.envelope.Segments[segmentStarts[index]:segmentEnds[index]] {
			if segment.Origin == OriginCurrentUser {
				currentText = append(currentText, segment.Text)
			}
		}
	}
	if firstCurrent < 0 || !isContinuationOnly(strings.Join(currentText, "\n")) {
		b.discardLinkedCandidate()
		return
	}
	if previousUser < 0 {
		b.discardLinkedCandidate()
		return
	}
	for index := segmentStarts[previousUser]; index < segmentEnds[previousUser]; index++ {
		if b.envelope.Segments[index].Origin == OriginHistory {
			b.envelope.Segments[index].Linked = true
		}
	}
	b.promoteLinkedCandidate()
}

type currentInputKind uint8

const (
	currentInputNone currentInputKind = iota
	currentInputExplicitMessage
	currentInputTextBlock
	currentInputImplicitMessage
	currentInputScalar
)

func inputItemRole(item gjson.Result) string {
	if !item.IsObject() {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(item.Get("role").String()))
}

func currentInputKindForItem(item gjson.Result) currentInputKind {
	if !item.IsObject() {
		if item.Type == gjson.String {
			return currentInputScalar
		}
		return currentInputNone
	}
	if role := inputItemRole(item); role != "" {
		if role == "user" {
			return currentInputExplicitMessage
		}
		return currentInputNone
	}
	switch strings.ToLower(strings.TrimSpace(item.Get("type").String())) {
	case "input_text":
		return currentInputTextBlock
	case "", "message":
		return currentInputImplicitMessage
	default:
		return currentInputNone
	}
}

func selectCurrentInputItems(items []gjson.Result) []bool {
	selected := make([]bool, len(items))
	lastIndex := -1
	lastKind := currentInputNone
	for index, item := range items {
		if kind := currentInputKindForItem(item); kind != currentInputNone {
			lastIndex = index
			lastKind = kind
		}
	}
	if lastIndex < 0 {
		return selected
	}
	// A request that ends with assistant/tool interaction data is a tool
	// continuation, not a new direct user turn. Do not resurrect an older user
	// message as current evidence merely because it is the last user candidate
	// somewhere in the replayed conversation. Session metadata and agent replay
	// items intentionally do not close the direct prompt because Codex can append
	// those transport records after a newly entered user message.
	for index := lastIndex + 1; index < len(items); index++ {
		if inputItemClosesDirectPrompt(items[index]) {
			return selected
		}
	}
	selected[lastIndex] = true
	// Adjacent input_text items are content blocks of one direct Responses
	// prompt. Message objects and scalar forms are complete messages, so only
	// their final candidate is current.
	if lastKind == currentInputTextBlock {
		for index := lastIndex - 1; index >= 0; index-- {
			kind := currentInputKindForItem(items[index])
			if kind == currentInputTextBlock {
				selected[index] = true
				continue
			}
			if typedInputItemIsAttachment(items[index]) {
				continue
			}
			break
		}
	}
	return selected
}

func inputItemClosesDirectPrompt(item gjson.Result) bool {
	if !item.IsObject() {
		return false
	}
	switch inputItemRole(item) {
	case "assistant", "tool", "function":
		return true
	}
	switch strings.ToLower(strings.TrimSpace(item.Get("type").String())) {
	case "output_text", "refusal",
		"function_call_output", "tool_call_output", "local_shell_call_output", "shell_call_output",
		"apply_patch_call_output", "tool_search_output", "tool_search_call_output", "custom_tool_call_output",
		"mcp_tool_call_output", "computer_call_output", "mcp_call_output", "tool_result",
		"function_call", "tool_call", "local_shell_call", "shell_call", "apply_patch_call",
		"tool_search_call", "custom_tool_call", "mcp_tool_call", "mcp_call", "mcp_list_tools",
		"mcp_approval_request", "mcp_approval_response", "additional_tools", "code_interpreter_call",
		"computer_call", "file_search_call", "image_generation_call", "web_search_call", "tool_use":
		return true
	default:
		return false
	}
}

func previousUserCandidate(items []gjson.Result, before int) int {
	for index := before - 1; index >= 0; index-- {
		if currentInputKindForItem(items[index]) != currentInputNone {
			return index
		}
	}
	return -1
}

func typedInputItemIsAttachment(item gjson.Result) bool {
	if !item.IsObject() || inputItemRole(item) != "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(item.Get("type").String())) {
	case "input_file", "input_image", "image_url", "file", "image", "attachment", "computer_screenshot":
		return true
	default:
		return false
	}
}

func (b *envelopeBuilder) appendMessage(message gjson.Result, role string, currentUser bool) {
	origin := OriginHistory
	switch role {
	case "user":
		if currentUser {
			origin = OriginCurrentUser
		}
	case "system":
		origin = OriginSystem
	case "developer":
		origin = OriginDeveloper
	case "tool", "function":
		origin = OriginToolOutput
	}
	if role == "tool" || role == "function" {
		b.appendResult(OriginToolOutput, role, firstExistingResult(message, "content", "output", "text"))
	} else {
		b.appendResult(origin, role, firstExistingResult(message, "content", "text"))
	}
	b.appendToolArguments(message, role)
	b.appendAttachmentReferences(message, role)
}

func (b *envelopeBuilder) appendTypedInputItem(item gjson.Result, currentUser bool) {
	if !item.Exists() || item.Type == gjson.Null {
		return
	}
	if !item.IsObject() {
		origin := OriginHistory
		if currentUser {
			origin = OriginCurrentUser
		}
		b.appendResult(origin, "user", item)
		return
	}
	itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
	switch itemType {
	case "function_call_output", "tool_call_output", "local_shell_call_output", "shell_call_output",
		"apply_patch_call_output", "tool_search_output", "tool_search_call_output", "custom_tool_call_output",
		"mcp_tool_call_output", "computer_call_output", "mcp_call_output", "tool_result":
		b.appendResult(OriginToolOutput, "tool", firstExistingResult(item, "output", "content", "text"))
	case "function_call", "tool_call", "local_shell_call", "shell_call", "apply_patch_call",
		"tool_search_call", "custom_tool_call", "mcp_tool_call", "mcp_call", "mcp_list_tools",
		"mcp_approval_request", "mcp_approval_response", "additional_tools", "code_interpreter_call",
		"computer_call", "file_search_call", "image_generation_call", "web_search_call", "tool_use":
		b.appendToolArguments(item, "assistant")
	case "agent_message":
		// These are assistant/agent replay items from an existing Codex session.
		// Treating their content as the current user prompt makes fixed transport
		// labels such as "Payload:" look like user intent and creates false hits.
		b.appendAgentMessage(item)
	case "output_text", "refusal":
		b.appendResult(OriginHistory, "assistant", firstExistingResult(item, "content", "text", "output"))
	case "reasoning":
		b.appendResult(OriginSessionContext, "assistant", firstExistingResult(item, "summary", "content", "text"))
	case "summary_text", "compaction", "context_compaction":
		b.appendResult(OriginSessionContext, "context", firstExistingResult(item, "summary", "content", "text"))
	case "compaction_trigger", "item_reference":
		// Control/reference items carry no end-user prompt text.
		b.appendAttachmentReferences(item, "context")
	case "encrypted_content":
		// Opaque server-side encrypted continuation payload (gpt-5.6 store=false
		// reasoning). No end-user prompt text; recognized and skipped.
		return
	case "input_text", "message":
		origin := OriginHistory
		if currentUser {
			origin = OriginCurrentUser
		}
		b.appendResult(origin, "user", firstExistingResult(item, "content", "text"))
	case "input_file", "input_image", "image_url", "file", "image", "attachment", "computer_screenshot":
		b.appendAttachmentReferences(item, "user")
	default:
		// An untyped object is the legacy direct-input shape. Unknown typed
		// objects have no proven provenance contract, so they are omitted from
		// detector input and surfaced through a non-punitive adapter audit.
		if itemType == "" {
			origin := OriginHistory
			if currentUser {
				origin = OriginCurrentUser
			}
			b.appendResult(origin, "user", item)
			return
		}
		b.markAdapterUnclassified(itemType)
	}
}

const maxAdapterUnclassifiedTypes = 8

func (b *envelopeBuilder) markAdapterUnclassified(blockType string) {
	if b == nil || b.envelope == nil {
		return
	}
	b.envelope.AdapterUnclassified = true
	blockType = strings.ToLower(strings.TrimSpace(blockType))
	if blockType == "" || len(b.envelope.AdapterUnclassifiedTypes) >= maxAdapterUnclassifiedTypes {
		return
	}
	for _, existing := range b.envelope.AdapterUnclassifiedTypes {
		if existing == blockType {
			return
		}
	}
	b.envelope.AdapterUnclassifiedTypes = append(b.envelope.AdapterUnclassifiedTypes, blockType)
}

func recognizedEnvelopeContentBlockType(blockType string) bool {
	switch blockType {
	case "text", "input_text", "output_text", "refusal", "message",
		"reasoning", "summary_text", "compaction", "compaction_summary", "context_compaction",
		"thinking", "redacted_thinking",
		"input_file", "input_image", "image_url", "file", "image", "attachment", "document", "computer_screenshot",
		"function_call_output", "tool_call_output", "local_shell_call_output", "shell_call_output",
		"apply_patch_call_output", "tool_search_output", "tool_search_call_output", "custom_tool_call_output",
		"mcp_tool_call_output", "computer_call_output", "mcp_call_output", "tool_result",
		"function_call", "tool_call", "local_shell_call", "shell_call", "apply_patch_call",
		"tool_search_call", "custom_tool_call", "mcp_tool_call", "mcp_call", "mcp_list_tools",
		"mcp_approval_request", "mcp_approval_response", "additional_tools", "code_interpreter_call",
		"computer_call", "file_search_call", "image_generation_call", "web_search_call", "tool_use", "agent_message",
		"compaction_trigger", "item_reference", "encrypted_content":
		return true
	default:
		return false
	}
}

func (b *envelopeBuilder) appendAgentMessage(item gjson.Result) {
	start := len(b.envelope.Segments)
	b.appendResult(OriginHistory, "assistant", firstExistingResult(item, "content", "text", "output"))
	write := start
	for index := start; index < len(b.envelope.Segments); index++ {
		segment := b.envelope.Segments[index]
		segment.Text = stripAgentMessageEnvelope(segment.Text)
		if segment.Text == "" {
			continue
		}
		b.envelope.Segments[write] = segment
		write++
	}
	b.envelope.Segments = b.envelope.Segments[:write]
	b.appendAttachmentReferences(item, "assistant")
}

func stripAgentMessageEnvelope(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	hasCollaborationHeader := false
	payloadIndex := -1
	for index, line := range lines {
		lower := strings.ToLower(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(lower, "message type:"), strings.HasPrefix(lower, "task name:"), strings.HasPrefix(lower, "sender:"), strings.HasPrefix(lower, "recipient:"):
			hasCollaborationHeader = true
		case strings.HasPrefix(lower, "payload:"):
			payloadIndex = index
		}
		if payloadIndex >= 0 {
			break
		}
	}
	if !hasCollaborationHeader || payloadIndex < 0 {
		return text
	}
	firstLine := strings.TrimSpace(lines[payloadIndex])
	firstPayload := strings.TrimSpace(firstLine[len("Payload:"):])
	parts := make([]string, 0, len(lines)-payloadIndex)
	if firstPayload != "" {
		parts = append(parts, firstPayload)
	}
	for _, line := range lines[payloadIndex+1:] {
		if line = strings.TrimSpace(line); line != "" {
			parts = append(parts, line)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func (b *envelopeBuilder) appendToolArguments(result gjson.Result, role string) {
	if !result.Exists() || !result.IsObject() {
		return
	}
	appendArgument := func(argument gjson.Result) {
		if !argument.Exists() || argument.Type == gjson.Null {
			return
		}
		if argument.Type == gjson.String {
			b.append(OriginToolArguments, role, argument.String())
			return
		}
		b.append(OriginToolArguments, role, argument.Raw)
	}
	appendArgument(result.Get("arguments"))
	appendArgument(result.Get("function.arguments"))
	appendArgument(result.Get("input"))
	appendArgument(result.Get("action"))
	appendArgument(result.Get("query"))
	if calls := result.Get("tool_calls"); calls.IsArray() {
		for _, call := range calls.Array() {
			appendArgument(call.Get("function.arguments"))
			appendArgument(call.Get("arguments"))
		}
	}
}

func (b *envelopeBuilder) appendAttachmentReferences(result gjson.Result, role string) {
	if !result.Exists() || result.Type == gjson.Null {
		return
	}
	if result.IsArray() {
		for _, item := range result.Array() {
			b.appendAttachmentReferences(item, role)
		}
		return
	}
	if result.Type == gjson.String {
		b.append(OriginAttachmentRefs, role, result.String())
		return
	}
	if !result.IsObject() {
		return
	}
	result.ForEach(func(key, value gjson.Result) bool {
		switch strings.ToLower(strings.TrimSpace(key.String())) {
		case "file_id", "image_url", "url", "attachment_id", "file":
			if value.Type == gjson.String {
				b.append(OriginAttachmentRefs, role, value.String())
			} else if value.IsObject() {
				b.appendAttachmentReferences(value, role)
			}
		case "attachments", "source", "images", "input_images", "mask":
			b.appendAttachmentReferences(value, role)
		}
		return true
	})
}

func firstExistingResult(result gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		value := result.Get(path)
		if value.Exists() && value.Type != gjson.Null {
			return value
		}
	}
	return gjson.Result{}
}
