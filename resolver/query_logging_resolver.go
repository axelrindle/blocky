package resolver

import (
	"time"

	"github.com/0xERR0R/blocky/config"
	"github.com/0xERR0R/blocky/log"
	"github.com/0xERR0R/blocky/model"
	"github.com/0xERR0R/blocky/querylog"
	"github.com/0xERR0R/blocky/util"
	"github.com/avast/retry-go/v4"
	"github.com/miekg/dns"
)

const (
	cleanUpRunPeriod         = 12 * time.Hour
	queryLoggingResolverType = "query_logging"
	logChanCap               = 1000
	defaultFlushPeriod       = 30 * time.Second
)

// QueryLoggingResolver writes query information (question, answer, duration, ...)
type QueryLoggingResolver struct {
	configurable[*config.QueryLogConfig]
	NextResolver
	typed

	logChan chan *querylog.LogEntry
	writer  querylog.Writer
}

// NewQueryLoggingResolver returns a new resolver instance
func NewQueryLoggingResolver(cfg config.QueryLogConfig) ChainedResolver {
	logger := log.PrefixedLog(queryLoggingResolverType)

	var writer querylog.Writer

	err := retry.Do(
		func() error {
			var err error
			switch cfg.Type {
			case config.QueryLogTypeCsv:
				writer, err = querylog.NewCSVWriter(cfg.Target, false, cfg.LogRetentionDays)
			case config.QueryLogTypeCsvClient:
				writer, err = querylog.NewCSVWriter(cfg.Target, true, cfg.LogRetentionDays)
			case config.QueryLogTypeMysql:
				writer, err = querylog.NewDatabaseWriter("mysql", cfg.Target, cfg.LogRetentionDays, defaultFlushPeriod)
			case config.QueryLogTypePostgresql:
				writer, err = querylog.NewDatabaseWriter("postgresql", cfg.Target, cfg.LogRetentionDays, defaultFlushPeriod)
			case config.QueryLogTypeConsole:
				writer = querylog.NewLoggerWriter()
			case config.QueryLogTypeNone:
				writer = querylog.NewNoneWriter()
			}

			return err
		},
		retry.Attempts(uint(cfg.CreationAttempts)),
		retry.DelayType(retry.FixedDelay),
		retry.Delay(cfg.CreationCooldown.ToDuration()),
		retry.OnRetry(func(n uint, err error) {
			logger.Warnf(
				"Error occurred on query writer creation, retry attempt %d/%d: %v", n+1, cfg.CreationAttempts, err,
			)
		}))
	if err != nil {
		logger.Error("can't create query log writer, using console as fallback: ", err)

		writer = querylog.NewLoggerWriter()
		cfg.Type = config.QueryLogTypeConsole
	}

	logChan := make(chan *querylog.LogEntry, logChanCap)

	resolver := QueryLoggingResolver{
		configurable: withConfig(&cfg),
		typed:        withType(queryLoggingResolverType),

		logChan: logChan,
		writer:  writer,
	}

	go resolver.writeLog()

	if cfg.LogRetentionDays > 0 {
		go resolver.periodicCleanUp()
	}

	return &resolver
}

// triggers periodically cleanup of old log files
func (r *QueryLoggingResolver) periodicCleanUp() {
	ticker := time.NewTicker(cleanUpRunPeriod)
	defer ticker.Stop()

	for {
		<-ticker.C
		r.doCleanUp()
	}
}

func (r *QueryLoggingResolver) doCleanUp() {
	r.writer.CleanUp()
}

// Resolve logs the query, duration and the result
func (r *QueryLoggingResolver) Resolve(request *model.Request) (*model.Response, error) {
	logger := log.WithPrefix(request.Log, queryLoggingResolverType)

	start := time.Now()

	resp, err := r.next.Resolve(request)

	duration := time.Since(start).Milliseconds()

	if err == nil {
		select {
		case r.logChan <- r.createLogEntry(request, resp, start, duration):
		default:
			logger.Error("query log writer is too slow, log entry will be dropped")
		}
	}

	return resp, err
}

func (r *QueryLoggingResolver) createLogEntry(request *model.Request, response *model.Response,
	start time.Time, durationMs int64,
) *querylog.LogEntry {
	entry := querylog.LogEntry{
		Start:       start,
		ClientIP:    "0.0.0.0",
		ClientNames: []string{"none"},
	}

	for _, f := range r.cfg.Fields {
		switch f {
		case config.QueryLogFieldClientIP:
			entry.ClientIP = request.ClientIP.String()

		case config.QueryLogFieldClientName:
			entry.ClientNames = request.ClientNames

		case config.QueryLogFieldResponseReason:
			entry.ResponseReason = response.Reason
			entry.ResponseType = response.RType.String()
			entry.ResponseCode = dns.RcodeToString[response.Res.Rcode]

		case config.QueryLogFieldResponseAnswer:
			entry.Answer = util.AnswerToString(response.Res.Answer)

		case config.QueryLogFieldQuestion:
			entry.QuestionName = request.Req.Question[0].Name
			entry.QuestionType = dns.TypeToString[request.Req.Question[0].Qtype]

		case config.QueryLogFieldDuration:
			entry.DurationMs = durationMs
		}
	}

	return &entry
}

// write entry: if log directory is configured, write to log file
func (r *QueryLoggingResolver) writeLog() {
	for logEntry := range r.logChan {
		start := time.Now()

		r.writer.Write(logEntry)

		halfCap := cap(r.logChan) / 2 //nolint:gomnd

		// if log channel is > 50% full, this could be a problem with slow writer (external storage over network etc.)
		if len(r.logChan) > halfCap {
			r.log().WithField("channel_len",
				len(r.logChan)).Warnf("query log writer is too slow, write duration: %d ms", time.Since(start).Milliseconds())
		}
	}
}
