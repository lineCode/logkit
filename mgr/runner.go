package mgr

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/qiniu/log"
	"github.com/qiniu/logkit/cleaner"
	"github.com/qiniu/logkit/conf"
	"github.com/qiniu/logkit/parser"
	"github.com/qiniu/logkit/reader"
	"github.com/qiniu/logkit/router"
	"github.com/qiniu/logkit/sender"
	"github.com/qiniu/logkit/transforms"
	. "github.com/qiniu/logkit/utils/models"

	"github.com/json-iterator/go"
	"github.com/qiniu/pandora-go-sdk/base/reqerr"
)

type CleanInfo struct {
	enable bool
	logdir string
}

const (
	SpeedUp     = "up"
	SpeedDown   = "down"
	SpeedStable = "stable"

	RunnerRunning = "running"
	RunnerStopped = "stopped"
)

type Runner interface {
	Name() string
	Run()
	Stop()
	Cleaner() CleanInfo
	Status() RunnerStatus
}

type Resetable interface {
	Reset() error
}

type TokenRefreshable interface {
	TokenRefresh(AuthTokens) error
}

type StatusPersistable interface {
	StatusBackup()
	StatusRestore()
}

type LogExportRunner struct {
	RunnerInfo

	stopped      int32
	exitChan     chan struct{}
	reader       reader.Reader
	cleaner      *cleaner.Cleaner
	parser       parser.LogParser
	senders      []sender.Sender
	router       *router.Router
	transformers []transforms.Transformer

	rs      *RunnerStatus
	lastRs  *RunnerStatus
	rsMutex *sync.RWMutex

	meta *reader.Meta

	batchLen  int
	batchSize int
	lastSend  time.Time
}

const defaultSendIntervalSeconds = 60
const defaultMaxBatchSize = 2 * 1024 * 1024
const qiniulogHeadPatthern = "[1-9]\\d{3}/[0-1]\\d/[0-3]\\d [0-2]\\d:[0-6]\\d:[0-6]\\d(\\.\\d{6})?"

// NewRunner 创建Runner
func NewRunner(rc RunnerConfig, cleanChan chan<- cleaner.CleanSignal) (runner Runner, err error) {
	return NewLogExportRunner(rc, cleanChan, reader.NewReaderRegistry(), parser.NewParserRegistry(), sender.NewSenderRegistry())
}

func NewCustomRunner(rc RunnerConfig, cleanChan chan<- cleaner.CleanSignal, rr *reader.ReaderRegistry, ps *parser.ParserRegistry, sr *sender.SenderRegistry) (runner Runner, err error) {
	if ps == nil {
		ps = parser.NewParserRegistry()
	}
	if sr == nil {
		sr = sender.NewSenderRegistry()
	}
	if rr == nil {
		rr = reader.NewReaderRegistry()
	}
	if rc.MetricConfig != nil {
		return NewMetricRunner(rc, sr)
	}
	return NewLogExportRunner(rc, cleanChan, rr, ps, sr)
}

func NewRunnerWithService(info RunnerInfo, reader reader.Reader, cleaner *cleaner.Cleaner, parser parser.LogParser, transformers []transforms.Transformer,
	senders []sender.Sender, router *router.Router, meta *reader.Meta) (runner Runner, err error) {
	return NewLogExportRunnerWithService(info, reader, cleaner, parser, transformers, senders, router, meta)
}

func NewLogExportRunnerWithService(info RunnerInfo, reader reader.Reader, cleaner *cleaner.Cleaner, parser parser.LogParser,
	transformers []transforms.Transformer, senders []sender.Sender, router *router.Router, meta *reader.Meta) (runner *LogExportRunner, err error) {
	if info.MaxBatchSize <= 0 {
		info.MaxBatchSize = defaultMaxBatchSize
	}
	if info.MaxBatchInterval <= 0 {
		info.MaxBatchInterval = defaultSendIntervalSeconds
	}
	runner = &LogExportRunner{
		RunnerInfo: info,
		exitChan:   make(chan struct{}),
		lastSend:   time.Now(), // 上一次发送时间
		rs: &RunnerStatus{
			SenderStats:    make(map[string]StatsInfo),
			TransformStats: make(map[string]StatsInfo),
			lastState:      time.Now(),
			Name:           info.RunnerName,
			RunningStatus:  RunnerRunning,
		},
		lastRs: &RunnerStatus{
			SenderStats:    make(map[string]StatsInfo),
			TransformStats: make(map[string]StatsInfo),
			lastState:      time.Now(),
			Name:           info.RunnerName,
			RunningStatus:  RunnerRunning,
		},
		rsMutex: new(sync.RWMutex),
	}
	if reader == nil {
		err = errors.New("reader can not be nil")
		return
	}
	runner.reader = reader
	if meta == nil {
		err = errors.New("meta can not be nil")
		return
	}
	runner.meta = meta
	if cleaner == nil {
		log.Warnf("%v's cleaner was disabled", info.RunnerName)
	}
	runner.cleaner = cleaner
	if parser == nil {
		err = errors.New("parser can not be nil")
		return
	}
	runner.parser = parser

	runner.transformers = transformers

	if len(senders) < 1 {
		err = errors.New("senders can not be nil")
		return
	}
	runner.senders = senders
	runner.router = router
	runner.StatusRestore()
	return runner, nil
}

func NewLogExportRunner(rc RunnerConfig, cleanChan chan<- cleaner.CleanSignal, rr *reader.ReaderRegistry, ps *parser.ParserRegistry, sr *sender.SenderRegistry) (runner *LogExportRunner, err error) {
	runnerInfo := RunnerInfo{
		EnvTag:           rc.EnvTag,
		RunnerName:       rc.RunnerName,
		MaxBatchSize:     rc.MaxBatchSize,
		MaxBatchLen:      rc.MaxBatchLen,
		MaxBatchInterval: rc.MaxBatchInterval,
		MaxBatchTryTimes: rc.MaxBatchTryTimes,
	}
	if rc.ReaderConfig == nil {
		return nil, errors.New(rc.RunnerName + " readerConfig is nil")
	}
	if rc.SenderConfig == nil {
		return nil, errors.New(rc.RunnerName + " SenderConfig is nil")
	}
	if rc.ParserConf == nil {
		return nil, errors.New(rc.RunnerName + " ParserConf is nil")
	}
	rc.ReaderConfig[GlobalKeyName] = rc.RunnerName
	rc.ReaderConfig[KeyRunnerName] = rc.RunnerName
	if rc.ExtraInfo {
		rc.ReaderConfig[ExtraInfo] = Bool2String(rc.ExtraInfo)
	}
	for i := range rc.SenderConfig {
		rc.SenderConfig[i][KeyRunnerName] = rc.RunnerName
	}
	rc.ParserConf[KeyRunnerName] = rc.RunnerName
	//配置文件适配
	rc = Compatible(rc)
	var (
		rd reader.Reader
		cl *cleaner.Cleaner
	)
	mode := rc.ReaderConfig["mode"]
	if mode == reader.ModeCloudTrail {
		syncDir := rc.ReaderConfig[reader.KeySyncDirectory]
		if syncDir == "" {
			bucket, prefix, region, ak, sk, _ := reader.GetS3UserInfo(rc.ReaderConfig)
			syncDir = reader.GetDefualtSyncDir(bucket, prefix, region, ak, sk, rc.RunnerName)
		}
		rc.ReaderConfig[reader.KeyLogPath] = syncDir
		if len(rc.CleanerConfig) == 0 {
			rc.CleanerConfig = conf.MapConf{
				"delete_enable":       "true",
				"delete_interval":     "60",
				"reserve_file_number": "50",
			}
		}
	}
	meta, err := reader.NewMetaWithConf(rc.ReaderConfig)
	if err != nil {
		return nil, err
	}
	if len(rc.CleanerConfig) > 0 {
		rd, err = rr.NewReaderWithMeta(rc.ReaderConfig, meta, false)
		if err != nil {
			return nil, err
		}
		cl, err = cleaner.NewCleaner(rc.CleanerConfig, meta, cleanChan, meta.LogPath())
		if err != nil {
			return nil, err
		}
	} else {
		rd, err = rr.NewReaderWithMeta(rc.ReaderConfig, meta, false)
		if err != nil {
			return nil, err
		}
	}
	parser, err := ps.NewLogParser(rc.ParserConf)
	if err != nil {
		return nil, err
	}
	transformers := createTransformers(rc)
	senders := make([]sender.Sender, 0)
	for i, c := range rc.SenderConfig {
		if rc.ExtraInfo && c[KeySenderType] == TypePandora {
			//如果已经开启了，不要重复加
			c[KeyPandoraExtraInfo] = "false"
		}
		s, err := sr.NewSender(c, meta.FtSaveLogPath())
		if err != nil {
			return nil, err
		}
		senders = append(senders, s)
		delete(rc.SenderConfig[i], InnerUserAgent)
	}
	senderCnt := len(senders)
	router, err := router.NewSenderRouter(rc.Router, senderCnt)
	if err != nil {
		return nil, fmt.Errorf("runner %v add sender router error, %v", rc.RunnerName, err)
	}
	return NewLogExportRunnerWithService(runnerInfo, rd, cl, parser, transformers, senders, router, meta)
}

func createTransformers(rc RunnerConfig) []transforms.Transformer {
	transformers := make([]transforms.Transformer, 0)
	for idx := range rc.Transforms {
		tConf := rc.Transforms[idx]
		tp := tConf[transforms.KeyType]
		if tp == nil {
			log.Error("field type is empty")
			continue
		}
		strTP, ok := tp.(string)
		if !ok {
			log.Error("field type is not string")
			continue
		}
		creater, ok := transforms.Transformers[strTP]
		if !ok {
			log.Errorf("type %v of transformer not exist", strTP)
			continue
		}
		trans := creater()
		bts, err := jsoniter.Marshal(tConf)
		if err != nil {
			log.Errorf("type %v of transformer marshal config error %v", strTP, err)
			continue
		}
		err = jsoniter.Unmarshal(bts, trans)
		if err != nil {
			log.Errorf("type %v of transformer unmarshal config error %v", strTP, err)
			continue
		}
		//transformer初始化
		if trans, ok := trans.(transforms.Initialize); ok {
			err = trans.Init()
			if err != nil {
				log.Errorf("type %v of transformer init error %v", strTP, err)
				continue
			}
		}
		transformers = append(transformers, trans)
	}
	return transformers
}

// trySend 尝试发送数据，如果此时runner退出返回false，其他情况无论是达到最大重试次数还是发送成功，都返回true
func (r *LogExportRunner) trySend(s sender.Sender, datas []Data, times int) bool {
	if len(datas) <= 0 {
		return true
	}
	r.rsMutex.Lock()
	if _, ok := r.rs.SenderStats[s.Name()]; !ok {
		r.rs.SenderStats[s.Name()] = StatsInfo{}
	}
	info := r.rs.SenderStats[s.Name()]
	r.rsMutex.Unlock()
	cnt := 1
	for {
		// 至少尝试一次。如果任务已经停止，那么只尝试一次
		if cnt > 1 && atomic.LoadInt32(&r.stopped) > 0 {
			return false
		}
		err := s.Send(datas)
		se, ok := err.(*StatsError)
		if ok {
			err = se.ErrorDetail
			if se.Ft {
				r.rsMutex.Lock()
				r.rs.Lag.Ftlags = se.FtQueueLag
				r.rsMutex.Unlock()
			} else {
				if cnt > 1 {
					info.Errors -= se.Success
				} else {
					info.Errors += se.Errors
				}
				info.Success += se.Success
			}
		} else if err != nil {
			if cnt <= 1 {
				info.Errors += int64(len(datas))
			}
		} else {
			info.Success += int64(len(datas))
		}
		if err != nil {
			info.LastError = err.Error()
			//FaultTolerant Sender 正常的错误会在backupqueue里面记录，自己重试，此处无需重试
			if se.Ft && se.FtNotRetry {
				break
			}
			time.Sleep(time.Second)
			se, succ := err.(*reqerr.SendError)
			if succ {
				datas = sender.ConvertDatas(se.GetFailDatas())
				//无限重试的，除非遇到关闭
				if atomic.LoadInt32(&r.stopped) > 0 {
					return false
				}
				log.Errorf("Runner[%v] send error %v for %v times, failed datas %v will retry send it", r.RunnerName, se.Error(), cnt, len(datas))
				cnt++
				continue
			}
			if times <= 0 || cnt < times {
				cnt++
				continue
			}
			log.Errorf("Runner[%v] retry send %v times, but still error %v, discard datas %v ... total %v lines", r.RunnerName, cnt, err, datas, len(datas))
		}
		break
	}
	r.rsMutex.Lock()
	r.rs.SenderStats[s.Name()] = info
	r.rsMutex.Unlock()
	return true
}

func getSampleContent(line string, maxBatchSize int) string {
	if len(line) <= maxBatchSize {
		return line
	}
	if maxBatchSize <= 1024 {
		return line
	}
	return line[0:1024]
}

func (r *LogExportRunner) Run() {
	if r.cleaner != nil {
		go r.cleaner.Run()
	}
	defer close(r.exitChan)
	defer func() {
		// recover when runner is stopped
		if atomic.LoadInt32(&r.stopped) <= 0 {
			return
		}
		if r := recover(); r != nil {
			log.Errorf("recover when runner is stopped\npanic: %v\nstack: %s", r, debug.Stack())
		}
	}()

	tags := r.meta.GetTags()
	tags = MergeEnvTags(r.EnvTag, tags)
	tags = MergeExtraInfoTags(r.meta, tags)

	datasourceTag := r.meta.GetDataSourceTag()
	schemaErr := SchemaErr{Number: 0, Last: time.Unix(0, 0)}

	for {
		if atomic.LoadInt32(&r.stopped) > 0 {
			log.Debugf("Runner[%v] exited from run", r.Name())
			if atomic.LoadInt32(&r.stopped) < 2 {
				r.exitChan <- struct{}{}
			}
			return
		}
		// read data
		var lines, froms []string
		var readErr error
		var line string
		for !r.batchFullOrTimeout() {
			line, readErr = r.reader.ReadLine()
			if os.IsNotExist(readErr) {
				log.Errorf("Runner[%v] reader %s - error: %v, sleep 3 second...", r.Name(), r.reader.Name(), readErr)
				time.Sleep(3 * time.Second)
				break
			}
			if readErr != nil && readErr != io.EOF {
				log.Errorf("Runner[%v] reader %s - error: %v, sleep 1 second...", r.Name(), r.reader.Name(), readErr)
				time.Sleep(time.Second)
				break
			}
			if len(line) <= 0 {
				log.Debugf("Runner[%v] reader %s no more content fetched sleep 1 second...", r.Name(), r.reader.Name())
				time.Sleep(1 * time.Second)
				continue
			}
			r.rsMutex.Lock()
			r.rs.ReadDataSize += int64(len(line))
			r.rs.ReadDataCount++
			r.rsMutex.Unlock()
			lines = append(lines, line)
			if datasourceTag != "" {
				froms = append(froms, r.reader.Source())
			}
			r.batchLen++
			r.batchSize += len(line)
		}
		r.rsMutex.Lock()
		if readErr != nil && readErr != io.EOF {
			if os.IsNotExist(readErr) {
				r.rs.ReaderStats.LastError = "no more file exist to be read"
			} else {
				r.rs.ReaderStats.LastError = readErr.Error()
			}
		} else {
			r.rs.ReaderStats.LastError = ""
		}
		r.rs.ReaderStats.Success = int64(r.batchLen)
		r.rsMutex.Unlock()

		r.batchLen = 0
		r.batchSize = 0
		r.lastSend = time.Now()

		for i := range r.transformers {
			var err error
			if r.transformers[i].Stage() == transforms.StageBeforeParser {
				lines, err = r.transformers[i].RawTransform(lines)
				if err != nil {
					log.Error(err)
				}
			}
		}

		if len(lines) <= 0 {
			log.Debugf("Runner[%v] fetched 0 lines", r.Name())
			_, ok := r.parser.(parser.Flushable)
			if ok {
				lines = []string{parser.PandoraParseFlushSignal}
			} else {
				continue
			}
		}

		// parse data
		errorCnt := int64(0)
		datas, err := r.parser.Parse(lines)
		se, ok := err.(*StatsError)
		r.rsMutex.Lock()
		if ok {
			errorCnt = se.Errors
			err = se.ErrorDetail
			r.rs.ParserStats.Errors += se.Errors
			r.rs.ParserStats.Success += se.Success
		} else if err != nil {
			errorCnt = 1
			r.rs.ParserStats.Errors++
		} else {
			r.rs.ParserStats.Success++
		}
		if err != nil {
			r.rs.ParserStats.LastError = err.Error()
		}
		r.rsMutex.Unlock()
		if err != nil {
			errMsg := fmt.Sprintf("Runner[%v] parser %s error : %v ", r.Name(), r.parser.Name(), err.Error())
			log.Debugf(errMsg)
			schemaErr.Output(errorCnt, errors.New(errMsg))
		}
		// send data
		if len(datas) <= 0 {
			log.Debugf("Runner[%v] received parsed data length = 0", r.Name())
			continue
		}

		//把datasourcetag加到data里，前提是认为[]line变成[]data以后是一一对应的，一旦错位就不加
		if datasourceTag != "" {
			//只要实际解析后数据比froms小就可以填上
			if len(datas) <= len(froms) {
				datas = addSourceToData(froms, se, datas, datasourceTag, r.Name())
			} else {
				var selen int
				if se != nil {
					selen = len(se.DatasourceSkipIndex)
				}
				log.Errorf("Runner[%v] datasourcetag add error, datas(TOTAL %v), datasourceSkipIndex(TOTAL %v) not match with froms(TOTAL %v)", r.Name(), len(datas), selen, len(froms))
				log.Debugf("Runner[%v] datasourcetag add error, datas %v datasourceSkipIndex %v froms %v", datas, se.DatasourceSkipIndex, froms)
			}
		}
		if len(tags) > 0 {
			datas = addTagsToData(tags, datas, r.Name())
		}
		for i := range r.transformers {
			if r.transformers[i].Stage() != transforms.StageAfterParser {
				continue
			}
			datas, err = r.transformers[i].Transform(datas)
			tp := r.transformers[i].Type()
			r.rsMutex.Lock()
			tstats, ok := r.rs.TransformStats[tp]
			if !ok {
				tstats = StatsInfo{}
			}
			se, ok := err.(*StatsError)
			if ok {
				errorCnt = se.Errors
				err = se.ErrorDetail
				tstats.Errors += se.Errors
				tstats.Success += se.Success
			} else if err != nil {
				errorCnt = 1
				tstats.Errors++
			} else {
				tstats.Success++
			}
			if err != nil {
				tstats.LastError = err.Error()
			}
			r.rs.TransformStats[tp] = tstats
			r.rsMutex.Unlock()
			if err != nil {
				log.Error(err)
			}
		}
		success := true
		senderCnt := len(r.senders)
		log.Debugf("Runner[%v] reader %s start to send at: %v", r.Name(), r.reader.Name(), time.Now().Format(time.RFC3339))
		senderDataList := classifySenderData(datas, r.router, senderCnt)
		for index, s := range r.senders {
			if !r.trySend(s, senderDataList[index], r.MaxBatchTryTimes) {
				success = false
				log.Errorf("Runner[%v] failed to send data finally", r.Name())
				break
			}
		}
		if success {
			r.reader.SyncMeta()
		}
		log.Debugf("Runner[%v] send %s finish to send at: %v", r.Name(), r.reader.Name(), time.Now().Format(time.RFC3339))
	}
}

func classifySenderData(datas []Data, router *router.Router, senderCnt int) [][]Data {
	senderDataList := make([][]Data, senderCnt)
	for i := 0; i < senderCnt; i++ {
		if router == nil {
			senderDataList[i] = datas
		} else {
			senderDataList[i] = make([]Data, 0)
		}
	}
	if router == nil {
		return senderDataList
	}
	for _, d := range datas {
		senderIndex := router.GetSenderIndex(d)
		senderData := senderDataList[senderIndex]
		senderData = append(senderData, d)
		senderDataList[senderIndex] = senderData
	}
	return senderDataList
}

func addSourceToData(sourceFroms []string, se *StatsError, datas []Data, datasourceTagName, runnername string) []Data {
	j := 0
	eql := len(sourceFroms) == len(datas)
	for i, v := range sourceFroms {
		if eql {
			j = i
		} else {
			if se != nil && se.ErrorIndexIn(i) {
				continue
			}
		}
		if j >= len(datas) {
			continue
		}

		if dt, ok := datas[j][datasourceTagName]; ok {
			log.Debugf("Runner[%v] datasource tag already has data %v, ignore %v", runnername, dt, v)
		} else {
			datas[j][datasourceTagName] = v
		}
		j++
	}
	return datas
}

func addTagsToData(tags map[string]interface{}, datas []Data, runnername string) []Data {
	for j, data := range datas {
		for k, v := range tags {
			if dt, ok := data[k]; ok {
				log.Debugf("Runner[%v] datasource tag already has data %v, ignore %v", runnername, dt, v)
			} else {
				data[k] = v
			}
		}
		datas[j] = data
	}
	return datas
}

func (r *LogExportRunner) Stop() {
	atomic.AddInt32(&r.stopped, 1)

	log.Warnf("Runner[%v] waiting for stopped signal", r.Name())
	timer := time.NewTimer(time.Second * 10)
	select {
	case <-r.exitChan:
		log.Warnf("runner " + r.Name() + " has been stopped ")
	case <-timer.C:
		log.Warnf("runner " + r.Name() + " exited timeout ")
		atomic.AddInt32(&r.stopped, 1)
	}
	log.Warnf("Runner[%v] wait for reader %v stopped", r.Name(), r.reader.Name())
	// 清理所有使用到的资源
	err := r.reader.Close()
	if err != nil {
		log.Errorf("Runner[%v] cannot close reader name: %s, err: %v", r.Name(), r.reader.Name(), err)
	} else {
		log.Warnf("Runner[%v] reader %v of runner %v closed", r.Name(), r.reader.Name(), r.Name())
	}
	for _, s := range r.senders {
		err := s.Close()
		if err != nil {
			log.Errorf("Runner[%v] cannot close sender name: %s, err: %v", r.Name(), s.Name(), err)
		} else {
			log.Warnf("Runner[%v] sender %v closed", r.Name(), s.Name())
		}
	}
	if r.cleaner != nil {
		r.cleaner.Close()
	}
}

func (r *LogExportRunner) Name() string {
	return r.RunnerName
}

func (r *LogExportRunner) Reset() (err error) {
	var errMsg string
	if read, ok := r.reader.(Resetable); ok {
		if subErr := read.Reset(); subErr != nil {
			errMsg += subErr.Error() + "\n"
		}
	}
	if err = r.meta.Reset(); err != nil {
		errMsg += err.Error() + "\n"
	}
	for _, sd := range r.senders {
		ssd, ok := sd.(Resetable)
		if ok {
			if nerr := ssd.Reset(); nerr != nil {
				errMsg += nerr.Error() + "\n"
			}
		}
	}
	if errMsg != "" {
		err = errors.New(errMsg)
	}
	return err
}

func (r *LogExportRunner) Cleaner() CleanInfo {
	if r.cleaner == nil {
		return CleanInfo{enable: false}
	}
	return CleanInfo{
		enable: true,
		logdir: r.cleaner.LogDir(),
	}
}

func (r *LogExportRunner) batchFullOrTimeout() bool {
	// 达到最大行数
	if r.MaxBatchLen > 0 && r.batchLen >= r.MaxBatchLen {
		log.Debugf("Runner[%v] meet the max batch length %v", r.RunnerName, r.MaxBatchLen)
		return true
	}
	// 达到最大字节数
	if r.MaxBatchSize > 0 && r.batchSize >= r.MaxBatchSize {
		log.Debugf("Runner[%v] meet the max batch size %v", r.RunnerName, r.MaxBatchSize)
		return true
	}
	// 超过最长的发送间隔
	if time.Now().Sub(r.lastSend).Seconds() >= float64(r.MaxBatchInterval) {
		log.Debugf("Runner[%v] meet the max batch send interval %v", r.RunnerName, r.MaxBatchInterval)
		return true
	}
	// 如果任务已经停止
	if atomic.LoadInt32(&r.stopped) > 0 {
		log.Warnf("Runner[%v] meet the stopped signal", r.RunnerName)
		return true
	}
	return false
}

func (r *LogExportRunner) LagStats() (rl *LagInfo, err error) {
	lr, ok := r.reader.(reader.LagReader)
	if ok {
		return lr.Lag()
	}
	//接口不支持，不显示错误比较好，有限reader就是不存在lag的概念的。
	rl = &LagInfo{}
	return
}

func getTrend(old, new float64) string {
	if old < new-0.1 {
		return SpeedUp
	}
	if old > new+0.1 {
		return SpeedDown
	}
	return SpeedStable
}

func (r *LogExportRunner) getStatusFrequently(now time.Time) (bool, float64, RunnerStatus) {
	r.rsMutex.RLock()
	defer r.rsMutex.RUnlock()
	elaspedTime := now.Sub(r.rs.lastState).Seconds()
	if elaspedTime <= 3 {
		return true, elaspedTime, r.lastRs.Clone()
	}
	return false, elaspedTime, RunnerStatus{}
}

func (r *LogExportRunner) Status() (rs RunnerStatus) {
	var isFre bool
	var elaspedtime float64
	now := time.Now()
	if isFre, elaspedtime, rs = r.getStatusFrequently(now); isFre {
		return rs
	}
	sts := r.getRefreshStatus(elaspedtime)
	return sts
}

func (r *LogExportRunner) getRefreshStatus(elaspedtime float64) RunnerStatus {
	now := time.Now()
	r.rsMutex.Lock()
	defer r.rsMutex.Unlock()
	r.rs.Error = ""
	r.rs.Logpath = r.meta.LogPath()
	rl, err := r.LagStats()
	if err != nil {
		r.rs.Error = fmt.Sprintf("get lag error: %v", err)
		log.Warn(r.rs.Error)
	}
	if rl != nil {
		r.rs.Lag = *rl
	}

	r.rs.Elaspedtime += elaspedtime
	r.rs.lastState = now
	for i := range r.transformers {
		newtsts := r.transformers[i].Stats()
		ttp := r.transformers[i].Type()
		if oldtsts, ok := r.lastRs.TransformStats[ttp]; ok {
			newtsts.Speed, newtsts.Trend = calcSpeedTrend(oldtsts, newtsts, elaspedtime)
		} else {
			newtsts.Speed, newtsts.Trend = calcSpeedTrend(StatsInfo{}, newtsts, elaspedtime)
		}
		r.rs.TransformStats[ttp] = newtsts
	}

	/*
		此处先不用reader的status, Run函数本身对这个ReaderStats赋值
		if str, ok := r.reader.(reader.StatsReader); ok {
			r.rs.ReaderStats = str.Status()
		}
	*/

	r.rs.ReadSpeedKB = float64(r.rs.ReadDataSize-r.lastRs.ReadDataSize) / elaspedtime
	r.rs.ReadSpeedTrendKb = getTrend(r.lastRs.ReadSpeedKB, r.rs.ReadSpeedKB)
	r.rs.ReadSpeed = float64(r.rs.ReadDataCount-r.lastRs.ReadDataCount) / elaspedtime
	r.rs.ReadSpeedTrend = getTrend(r.lastRs.ReadSpeed, r.rs.ReadSpeed)
	r.rs.ReaderStats.Speed = r.rs.ReadSpeed
	r.rs.ReaderStats.Trend = r.rs.ReadSpeedTrend
	r.rs.ReaderStats.Success = r.rs.ReadDataCount

	r.rs.ParserStats.Speed, r.rs.ParserStats.Trend = calcSpeedTrend(r.lastRs.ParserStats, r.rs.ParserStats, elaspedtime)

	for i := range r.senders {
		sts, ok := r.senders[i].(sender.StatsSender)
		if ok {
			r.rs.SenderStats[r.senders[i].Name()] = sts.Stats()
		}
	}

	for k, v := range r.rs.SenderStats {
		if lv, ok := r.lastRs.SenderStats[k]; ok {
			v.Speed, v.Trend = calcSpeedTrend(lv, v, elaspedtime)
		} else {
			v.Speed, v.Trend = calcSpeedTrend(StatsInfo{}, v, elaspedtime)
		}
		r.rs.SenderStats[k] = v
	}
	r.rs.RunningStatus = RunnerRunning
	*r.lastRs = r.rs.Clone()
	return *r.lastRs
}

func calcSpeedTrend(old, new StatsInfo, elaspedtime float64) (speed float64, trend string) {
	if elaspedtime < 0.001 {
		speed = old.Speed
	} else {
		speed = float64(new.Success-old.Success) / elaspedtime
	}
	trend = getTrend(old.Speed, speed)
	return
}

func deepCopyByJson(dst, src interface{}) {
	var err error
	var confByte []byte
	if confByte, err = jsoniter.Marshal(src); err != nil {
		log.Errorf("deepCopyByJson marshal error %v, use same pointer", err)
		dst = src
		return
	}
	if err = jsoniter.Unmarshal(confByte, dst); err != nil {
		log.Errorf("deepCopyByJson unmarshal error %v, use same pointer", err)
		dst = src
		return
	}
	return
}

//Compatible 用于新老配置的兼容
func Compatible(rc RunnerConfig) RunnerConfig {
	//兼容qiniulog与reader多行的配置
	if rc.ParserConf == nil {
		return rc
	}
	if rc.ReaderConfig == nil {
		return rc
	}
	parserType, err := rc.ParserConf.GetString(parser.KeyParserType)
	if err != nil {
		return rc
	}
	pattern, _ := rc.ReaderConfig.GetStringOr(reader.KeyHeadPattern, "")
	if parserType == parser.TypeLogv1 && pattern == "" {
		prefix, _ := rc.ParserConf.GetStringOr(parser.KeyQiniulogPrefix, "")
		prefix = strings.TrimSpace(prefix)
		var readpattern string
		if len(prefix) > 0 {
			readpattern = "^" + prefix + " " + qiniulogHeadPatthern
		} else {
			readpattern = "^" + qiniulogHeadPatthern
		}
		rc.ReaderConfig[reader.KeyHeadPattern] = readpattern
	}
	return rc
}

func (r *LogExportRunner) TokenRefresh(tokens AuthTokens) error {
	if r.RunnerName != tokens.RunnerName {
		return fmt.Errorf("tokens.RunnerName[%v] is not match %v", tokens.RunnerName, r.RunnerName)
	}
	if len(r.senders) > tokens.SenderIndex {
		if tokenSender, ok := r.senders[tokens.SenderIndex].(sender.TokenRefreshable); ok {
			return tokenSender.TokenRefresh(tokens.SenderTokens)
		}
	}
	return nil
}

func (r *LogExportRunner) StatusRestore() {
	rStat, err := r.meta.ReadStatistic()

	if err != nil {
		log.Warnf("runner %v, restore status failed", r.RunnerName)
		return
	}
	r.rs.ReadDataCount = rStat.ReaderCnt
	r.rs.ParserStats.Success = rStat.ParserCnt[0]
	r.rs.ParserStats.Errors = rStat.ParserCnt[1]
	for _, s := range r.senders {
		name := s.Name()
		info, exist := rStat.SenderCnt[name]
		if !exist {
			continue
		}
		sStatus, ok := s.(sender.StatsSender)
		if ok {
			sStatus.Restore(&StatsInfo{
				Success: info[0],
				Errors:  info[1],
			})
		}
		status, ext := r.rs.SenderStats[name]
		if !ext {
			status = StatsInfo{}
		}
		status.Success = info[0]
		status.Errors = info[1]
		r.rs.SenderStats[name] = status
	}
	*r.lastRs = r.rs.Clone()
	log.Infof("runner %v restore status %v", r.RunnerName, rStat)
}

func (r *LogExportRunner) StatusBackup() {
	status := r.Status()
	bStart := &reader.Statistic{
		ReaderCnt: status.ReadDataCount,
		ParserCnt: [2]int64{
			status.ParserStats.Success,
			status.ParserStats.Errors,
		},
		SenderCnt: map[string][2]int64{},
	}
	for _, s := range r.senders {
		name := s.Name()
		sStatus, ok := s.(sender.StatsSender)
		if ok {
			status.SenderStats[name] = sStatus.Stats()
		}
		if sta, exist := status.SenderStats[name]; exist {
			bStart.SenderCnt[name] = [2]int64{
				sta.Success,
				sta.Errors,
			}
		}
	}
	err := r.meta.WriteStatistic(bStart)
	if err != nil {
		log.Warnf("runner %v, backup status failed", r.RunnerName)
	} else {
		log.Infof("runner %v, backup status %v", r.RunnerName, bStart)
	}
}

// MergeEnvTags 获取环境变量里的内容
func MergeEnvTags(name string, tags map[string]interface{}) map[string]interface{} {
	if name == "" {
		return tags
	}

	envTags := make(map[string]interface{})
	if value := os.Getenv(name); value != "" {
		err := jsoniter.Unmarshal([]byte(value), &envTags)
		if err != nil {
			log.Warnf("get env tags error: %v", err)
			return tags
		}
	}

	if tags == nil {
		tags = make(map[string]interface{})
	}
	for k, v := range envTags {
		tags[k] = v
	}
	return tags
}

func MergeExtraInfoTags(meta *reader.Meta, tags map[string]interface{}) map[string]interface{} {
	if tags == nil {
		tags = make(map[string]interface{})
	}
	for k, v := range meta.ExtraInfo() {
		if _, ok := tags[k]; !ok {
			tags[k] = v
		}
	}
	return tags
}
