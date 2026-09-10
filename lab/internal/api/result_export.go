package api

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/pfap/lab/internal/model"
)

type metricDefinition struct{ Key, Name, Unit, Explanation string }

var exportDefinitions = []metricDefinition{
	{"windowSeconds", "实际观察窗口", "s", "测量开始至计划截止、提前停止、导出时刻三者中最早者；预热和收尾不延长分母。"},
	{"measurementSubmissions", "正式提交批次", "笔", "仅 RunPhase=measuring 的本次运行交易；不包括账户准备、手动交易和预热提交。"},
	{"confirmedSubmissions", "正式批次成功", "笔", "正式提交批次中已成功确认且双方核验完成的交易；允许在收尾阶段完成。"},
	{"pendingSubmissions", "正式批次未完成", "笔", "包含 queued/proving/submitted/unknown/settling；未完成记录不能当成成功或从成功率分母消失。"},
	{"failedSubmissions", "正式批次失败或取消", "笔", "正式提交批次中的 failed/timeout/cancelled。"},
	{"windowConfirmed", "窗口观察确认数", "笔", "本次运行在窗口内观察到成功 Receipt 且最终双方核验通过的数量，可包含预热提交。未完成运行的结果可能补全。"},
	{"observedConfirmedTPS", "窗口确认吞吐", "TPS", "窗口观察确认数 / 实际观察秒数。按控制器观察 Receipt 的时间归窗，不是精确打包时刻。"},
	{"meanLatencyMs", "平均端到端延迟", "ms", "正式批次成功交易的 ConfirmedAt-SubmittedAt 均值；包含准入后等待与证明阶段，不包含账户准备。"},
	{"p95LatencyMs", "端到端 p95", "ms", "升序样本 x，以零基下标 floor((n-1)*0.95) 取值，不插值；与页面口径一致，不跨运行平均分位数。"},
	{"p99LatencyMs", "端到端 p99", "ms", "升序样本以零基下标 floor((n-1)*0.99) 取值，不插值；缺失时不填 0。"},
	{"meanChainLatencyMs", "平均链上确认延迟", "ms", "正式批次中有 BroadcastAt 的成功交易，ConfirmedAt-BroadcastAt 的均值；包含轮询观察误差。"},
	{"chainLatencyP95Ms", "链上确认 p95", "ms", "相同链上确认样本的 p95。"},
	{"readyCycleP95Ms", "双方就绪周期 p95", "ms", "正式批次成功且有 ReadyAt 的样本，ReadyAt-SubmittedAt 的 p95；区别于首次成功 Receipt。"},
	{"count", "观察区块数", "块", "新版观察器每 5 秒采集达到运行确认深度的连续规范链区块，按控制器观察时间归窗，包含深度与轮询延迟；正式窗口的已确认锚重组或漏采使窗口无效。旧运行以其保存的采集记录和异常为准，不追溯改写。"},
	{"meanSizeB", "平均区块编码大小", "B", "窗口内全部已采集区块 Size 均值，包含空块。"},
	{"meanNonemptySizeB", "非空块平均大小", "B", "仅 Transactions>0 的窗口区块编码大小均值。"},
	{"meanBlockIntervalSeconds", "平均出块间隔", "s", "窗口连续样本末块与首块的头时间戳之差 / (区块数-1)；少于两块不计算。"},
	{"meanExecutionValidationUs", "平均执行与状态验证", "µs", "观察节点 geth 的 Process+ValidateState 微秒埋点均值；不含共识头验证、网络和数据库提交，不等于完整区块验证。"},
	{"validationSamples", "区块验证有效样本", "块", "具有明确埋点的窗口区块数。本地挖出的区块可能不经过该导入路径。"},
	{"missingValidationSamples", "区块验证缺失样本", "块", "窗口区块数减有效埋点数；旧运行包或日志未覆盖可能造成缺失。"},
}

type exportMetric struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Unit        string `json:"unit"`
	Value       any    `json:"value"`
	Samples     int    `json:"samples"`
	Explanation string `json:"explanation"`
}
type exportedRun struct {
	Configuration map[string]any   `json:"configuration"`
	Metrics       []exportMetric   `json:"metrics"`
	Complete      bool             `json:"completeWindow"`
	Minutes       any              `json:"minutes"`
	Blocks        []model.RunBlock `json:"blocks"`
	Limitations   any              `json:"limitations"`
}
type exportedResults struct {
	BuildMetricDefinitions map[string]string `json:"buildMetricDefinitions"`
	SchemaVersion          int               `json:"schemaVersion"`
	GeneratedAt            time.Time         `json:"generatedAt"`
	Scope                  string            `json:"scope"`
	Experiment             map[string]any    `json:"experiment"`
	Runs                   []exportedRun     `json:"runs"`
	Transactions           []map[string]any  `json:"transactions"`
	Events                 []map[string]any  `json:"events"`
	BuildProfile           map[string]any    `json:"buildProfile"`
	Notes                  []string          `json:"notes"`
}

func object(v any) map[string]any {
	b, _ := json.Marshal(v)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}
func fields(m map[string]any, names string) map[string]any {
	out := map[string]any{}
	for _, key := range strings.Fields(names) {
		if v, ok := m[key]; ok {
			out[key] = v
		}
	}
	return out
}
func safeNode(v any) map[string]any {
	return fields(object(v), "id name serverId index localIndex status isMiner mining block peers lastSeen runtimeSha publicBalance")
}
func safeSystemInfo(value any) map[string]any {
	out := map[string]any{}
	text, _ := value.(string)
	for _, line := range strings.Split(text, "\n") {
		key, v, ok := strings.Cut(line, "=")
		if ok && strings.Contains(" host kernel cpus memory_kb memory_available_kb load1 ", " "+key+" ") {
			out[key] = v
		}
	}
	return out
}
func safeConfiguration(c map[string]any) map[string]any {
	out := fields(c, "networkId minerCount minerMode artifactSha recoveryArtifactSha capturedAt scheduler blockSampling blockSamplingConfirmations blockWarmupReorgs blockWarmupResetAt admissionSamples admissionSampleAt")
	for _, key := range []string{"nodes", "servers"} {
		list := []map[string]any{}
		b, _ := json.Marshal(c[key])
		var input []map[string]any
		_ = json.Unmarshal(b, &input)
		for _, x := range input {
			if key == "nodes" {
				list = append(list, safeNode(x))
			} else {
				server := fields(x, "id host hostGroup p2pHost")
				server["system"] = safeSystemInfo(x["systemInfo"])
				list = append(list, server)
			}
		}
		out[key] = list
	}
	return out
}
func safeRun(w model.Workload) map[string]any {
	out := fields(object(w), "id experimentId name type value strategy mode nodeIds observerNodeId warmupSeconds durationSeconds confirmations ratePerSecond phase status submitted attempted skippedBusy skippedUnavailable stopRequested createdAt startedAt measurementStartedAt measurementEndsAt submissionStoppedAt finishedAt")
	out["initialConfiguration"] = safeConfiguration(w.Configuration)
	out["hasAnomaly"] = w.Error != "" || w.InvalidReason != "" || w.BlockError != ""
	out["anomalyFlags"] = map[string]bool{"submissionStopped": w.StopRequested, "windowInvalid": w.InvalidReason != "", "blockCollectionAnomaly": w.BlockError != "", "executionAnomaly": w.Error != ""}
	return out
}

func exportedRunMetrics(s model.State, w model.Workload, now time.Time) exportedRun {
	r := runReport(s, w, now)
	blocks := r["blocks"].(map[string]any)
	chainSamples, readySamples := 0, 0
	var chainSum float64
	cohort := []model.Transaction{}
	for _, t := range s.Transactions {
		if t.WorkloadID == w.ID && t.RunPhase == "measuring" && t.Status == "confirmed" {
			cohort = append(cohort, t)
			if !t.BroadcastAt.IsZero() {
				chainSamples++
				chainSum += float64(t.ConfirmedAt.Sub(t.BroadcastAt).Microseconds()) / 1000
			}
			if !t.ReadyAt.IsZero() {
				readySamples++
			}
		}
	}
	if chainSamples > 0 {
		r["meanChainLatencyMs"] = chainSum / float64(chainSamples)
	}
	rows := []exportMetric{}
	for _, d := range exportDefinitions {
		value := r[d.Key]
		samples := len(cohort)
		if v, ok := blocks[d.Key]; ok {
			value = v
			samples = blocks["count"].(int)
		}
		switch d.Key {
		case "windowSeconds":
			samples = 1
		case "measurementSubmissions", "confirmedSubmissions", "pendingSubmissions", "failedSubmissions":
			samples = r["measurementSubmissions"].(int)
		case "observedConfirmedTPS", "windowConfirmed":
			samples = r["windowConfirmed"].(int)
		case "meanBlockIntervalSeconds":
			samples = blocks["count"].(int) - 1
			if samples < 0 {
				samples = 0
			}
		case "meanExecutionValidationUs":
			samples = blocks["validationSamples"].(int)
		case "chainLatencyP95Ms", "meanChainLatencyMs":
			samples = chainSamples
		case "readyCycleP95Ms":
			samples = readySamples
		case "meanNonemptySizeB":
			samples = 0
			for _, b := range w.Blocks {
				if b.Transactions > 0 && !b.ObservedAt.Before(w.MeasurementStartedAt) && b.ObservedAt.Before(w.MeasurementStartedAt.Add(time.Duration(r["windowSeconds"].(float64)*float64(time.Second)))) {
					samples++
				}
			}
		}
		if samples == 0 && (strings.Contains(d.Key, "Latency") || d.Key == "readyCycleP95Ms") {
			value = nil
		}
		if w.Strategy != "ready-pool" {
			value = nil
			samples = 0
		}
		rows = append(rows, exportMetric{d.Key, d.Name, d.Unit, value, samples, d.Explanation})
	}
	timingFields := []string{"proofDurationUs", "verifyDurationUs", "txGenerationUs", "txVerificationUs", "payerProofGenerationUs", "payerProofVerificationUs", "payerTxGenerationUs", "payerTxVerificationUs", "receiverProofGenerationUs", "receiverProofVerificationUs", "receiverTxGenerationUs", "receiverTxVerificationUs"}
	timingNames := []string{"证明生成均值", "证明验证均值", "交易生成估算均值", "交易验证均值", "付款方证明生成均值", "付款方证明验证均值", "付款方交易生成估算均值", "付款方交易验证均值", "收款方证明生成均值", "收款方证明验证均值", "收款方交易生成估算均值", "收款方交易验证均值"}
	for index, key := range timingFields {
		sum := float64(0)
		n := 0
		for _, t := range cohort {
			v, _ := object(t)[key].(float64)
			if v > 0 {
				sum += v
				n++
			}
		}
		var mean any
		if n > 0 {
			mean = sum / float64(n)
		}
		rows = append(rows, exportMetric{key, timingNames[index], "µs", mean, n, "正式批次成功交易的已采集正值均值；0/缺失不代替真实采样。payer=付款方、receiver=收款方；证明耗时来自节点日志，交易生成是 RPC 墙钟扣除证明生成/验证的估算，交易验证来自独立埋点。"})
	}
	limitations := append([]string{}, r["limitations"].([]string)...)
	if scope, ok := blocks["scope"].(string); ok && scope != "" {
		limitations = append(limitations, scope)
	}
	return exportedRun{safeRun(w), rows, r["valid"].(bool) && w.Strategy == "ready-pool", r["minuteSeries"], w.Blocks, limitations}
}

func buildResultExport(s model.State, eid, rid string, now time.Time) (exportedResults, bool) {
	out := exportedResults{SchemaVersion: 2, GeneratedAt: now, Scope: "experiment", Runs: []exportedRun{}, Transactions: []map[string]any{}, Events: []map[string]any{}, Notes: []string{"各次运行独立统计，不对 TPS 或分位数直接取平均。准备、手动与预热交易保留明细，但不混入正式提交批次。", "这是导出时刻的快照；运行中、提前停止、无样本和异常窗口不等于完整稳态结果。预热资格检查不是统计平稳性的证明。", "凭据、身份文件路径、执行命令、Receipt 原文、匿名 SN/证明/承诺、原始日志和自由文本错误/事件消息不导出；事件保留时间、级别、类别供定位。", "CSV 文本中可能触发表格公式的前导字符添加单引号保护；JSON 保留原始文本。", "构建档案是导出时控制器的当前档案，未必对应历史实验运行包；应对照各次运行的 SHA 快照，不追溯冒充历史构建记录。"}}
	out.BuildMetricDefinitions = map[string]string{"libsnarkBuildDurationMs": "电路库构建墙钟耗时，ms；不包含单独列出的密钥生成阶段。", "keyGenerationDurationMs": "全部密钥生成阶段墙钟耗时，ms；一次性构建成本，不计入交易窗口。", "keyDurationsMs": "CreateAccount、Mint、Redeem、Transfer 各自密钥生成程序耗时，ms。", "keys": "每个 pk/vk 文件 bytes 为实测文件大小，单位 B；sha256 用于文件一致性核对。", "host/kernel/cpuModel/cpus/memoryKb/compiler": "构建时主机与系统环境；memoryKb 为 KiB。档案归属以构建证据为准，不假设与历史运行同版本。"}
	if rid != "" {
		found := false
		for _, w := range s.Workloads {
			if w.ID == rid {
				eid = w.ExperimentID
				found = true
			}
		}
		if !found {
			return out, false
		}
		out.Scope = "run"
	}
	found := false
	for _, e := range s.Experiments {
		if e.ID == eid {
			found = true
			out.Experiment = fields(object(e), "id name status networkId topology minerCount minerMode minerSelections artifactSha recoveryArtifactSha placements createdAt startedAt finishedAt")
			nodes := []map[string]any{}
			for _, n := range e.Nodes {
				nodes = append(nodes, safeNode(n))
			}
			out.Experiment["nodes"] = nodes
			servers := []map[string]any{}
			for _, srv := range s.Servers {
				used := false
				for _, n := range e.Nodes {
					if n.ServerID == srv.ID {
						used = true
					}
				}
				for _, p := range e.Placements {
					if p.ServerID == srv.ID {
						used = true
					}
				}
				if used {
					x := fields(object(srv), "id name host hostGroup p2pHost status lastCheck")
					x["system"] = safeSystemInfo(srv.SystemInfo)
					servers = append(servers, x)
				}
			}
			out.Experiment["serversAtExport"] = servers
			out.Experiment["hasAnomaly"] = e.Error != "" || e.MiningError != ""
		}
	}
	if !found {
		return out, false
	}
	for _, w := range s.Workloads {
		if w.ExperimentID == eid && (rid == "" || w.ID == rid) {
			out.Runs = append(out.Runs, exportedRunMetrics(s, w, now))
		}
	}
	for _, t := range s.Transactions {
		if t.ExperimentID == eid && (rid == "" || t.WorkloadID == rid) {
			out.Transactions = append(out.Transactions, fields(object(t), "id experimentId workloadId batchId sequence type fromNode toNode value status runPhase submittedAt provingAt broadcastAt confirmedAt readyAt hash blockNumber receiptStatus proofDurationUs verifyDurationUs proofDurationMs verifyDurationMs txGenerationUs txVerificationUs payerProofGenerationUs payerProofVerificationUs payerTxGenerationUs payerTxVerificationUs receiverProofGenerationUs receiverProofVerificationUs receiverTxGenerationUs receiverTxVerificationUs"))
		}
	}
	if rid == "" {
		for _, event := range s.Events {
			if event.ExperimentID == eid {
				out.Events = append(out.Events, fields(object(event), "id experimentId at level kind"))
			}
		}
	}
	return out, true
}

func csvText(v any) string {
	if v == nil {
		return ""
	}
	var s string
	switch x := v.(type) {
	case string:
		s = x
	default:
		b, _ := json.Marshal(v)
		s = string(b)
	}
	trim := strings.TrimLeft(s, " \t\r\n")
	if len(trim) > 0 && strings.ContainsAny(trim[:1], "=+-@") || strings.HasPrefix(s, "\t") || strings.HasPrefix(s, "\r") {
		return "'" + s
	}
	return s
}
func writeCSV(z *zip.Writer, name string, rows []map[string]any) error {
	keys := map[string]bool{}
	for _, row := range rows {
		for k := range row {
			keys[k] = true
		}
	}
	columns := []string{}
	for k := range keys {
		columns = append(columns, k)
	}
	sort.Strings(columns)
	if len(columns) == 0 {
		columns = []string{"no_records"}
	}
	file, err := z.Create(name)
	if err != nil {
		return err
	}
	_, err = file.Write([]byte{0xef, 0xbb, 0xbf})
	if err != nil {
		return err
	}
	writer := csv.NewWriter(file)
	if err = writer.Write(columns); err != nil {
		return err
	}
	for _, row := range rows {
		values := []string{}
		for _, key := range columns {
			values = append(values, csvText(row[key]))
		}
		if err = writer.Write(values); err != nil {
			return err
		}
	}
	writer.Flush()
	return writer.Error()
}

var resultHTML = template.Must(template.New("report").Funcs(template.FuncMap{"json": func(v any) string { b, _ := json.MarshalIndent(v, "", "  "); return string(b) }, "value": func(v any) string {
	if v == nil {
		return "未采集 / 无适用样本"
	}
	return fmt.Sprint(v)
}}).Parse(`<!doctype html><html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>PFAP 实验报告</title><style>body{font:14px system-ui;line-height:1.6;color:#172c25;max-width:1200px;margin:32px auto;padding:0 20px}table{width:100%;border-collapse:collapse}td,th{border:1px solid #ccd8d2;padding:8px;text-align:left;overflow-wrap:anywhere}pre{white-space:pre-wrap;overflow-wrap:anywhere;background:#f2f6f4;padding:16px}section{margin:32px 0}small{color:#586c62}@media print{section{break-before:auto}tr{break-inside:avoid}body{max-width:none}}</style><h1>PFAP 实验结果报告</h1><p>范围：{{.Scope}} · 导出时间 {{.GeneratedAt}}</p><h2>{{index .Experiment "name"}}</h2>{{range .Notes}}<p>{{.}}</p>{{end}}<h2>实验配置</h2><pre>{{json .Experiment}}</pre>{{range .Runs}}<section><h2>{{index .Configuration "name"}}</h2><p>完整窗口：{{.Complete}} · 状态 {{index .Configuration "status"}}</p><pre>{{json .Configuration}}</pre><table><thead><tr><th>指标</th><th>数值</th><th>单位</th><th>有效样本</th><th>定义与口径</th></tr></thead><tbody>{{range .Metrics}}<tr><td>{{.Name}}</td><td>{{value .Value}}</td><td>{{.Unit}}</td><td>{{.Samples}}</td><td>{{.Explanation}}</td></tr>{{end}}</tbody></table><h3>分钟趋势</h3><pre>{{json .Minutes}}</pre><h3>限制说明</h3><pre>{{json .Limitations}}</pre></section>{{else}}<p>尚无自动运行记录；未生成虚构的稳态性能结果。</p>{{end}}<h2>构建与密钥档案（导出时当前档案）</h2><p>keyDurationsMs 为各电路 pk/vk 生成耗时（ms）；keys 中 bytes 为文件字节数（B）。缺失字段表示未记录。</p><pre>{{json .BuildProfile}}</pre><h2>交易明细</h2><pre>{{json .Transactions}}</pre><h2>事件索引（原文已排除）</h2><pre>{{json .Events}}</pre><p>可使用浏览器打印功能保存为 PDF；CSV ZIP 和 JSON 提供结构化数据。</p></html>`))

func (a *API) exportResults(w http.ResponseWriter, r *http.Request, eid, rid string) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "html" && format != "csv" {
		fail(w, 400, fmt.Errorf("format must be json, html or csv"))
		return
	}
	var out exportedResults
	var found bool
	a.store.View(func(s model.State) { out, found = buildResultExport(s, eid, rid, time.Now()) })
	if !found {
		fail(w, 404, os.ErrNotExist)
		return
	}
	if b, err := os.ReadFile("dist/build-profile.json"); err == nil {
		var p map[string]any
		if json.Unmarshal(b, &p) == nil {
			out.BuildProfile = fields(p, "recordedAt host kernel cpuModel cpus memoryKb compiler libsnarkBuildDurationMs keyGenerationDurationMs keyDurationsMs keys")
			keys := map[string]any{}
			durations := map[string]any{}
			rawKeys, _ := p["keys"].(map[string]any)
			rawDurations, _ := p["keyDurationsMs"].(map[string]any)
			for _, circuit := range []string{"createaccount", "mint", "redeem", "transfer"} {
				if n, ok := rawDurations[circuit].(float64); ok {
					durations[circuit] = n
				}
				for _, suffix := range []string{"pk.txt", "vk.txt"} {
					key := circuit + suffix
					if entry, ok := rawKeys[key].(map[string]any); ok {
						keys[key] = fields(entry, "bytes sha256")
					}
				}
			}
			out.BuildProfile["keys"] = keys
			out.BuildProfile["keyDurationsMs"] = durations
		}
	}
	var data bytes.Buffer
	contentType, extension := "application/json; charset=utf-8", "json"
	var err error
	switch format {
	case "html":
		contentType, extension = "text/html; charset=utf-8", "html"
		err = resultHTML.Execute(&data, out)
	case "csv":
		contentType, extension = "application/zip", "zip"
		z := zip.NewWriter(&data)
		metrics, blocks, minutes := []map[string]any{}, []map[string]any{}, []map[string]any{}
		for _, run := range out.Runs {
			for _, m := range run.Metrics {
				row := object(m)
				row["runId"] = run.Configuration["id"]
				metrics = append(metrics, row)
			}
			for _, b := range run.Blocks {
				row := object(b)
				row["runId"] = run.Configuration["id"]
				blocks = append(blocks, row)
			}
			raw, _ := json.Marshal(run.Minutes)
			var rows []map[string]any
			_ = json.Unmarshal(raw, &rows)
			for _, row := range rows {
				row["runId"] = run.Configuration["id"]
				minutes = append(minutes, row)
			}
		}
		for _, file := range []struct {
			name string
			rows []map[string]any
		}{{"metrics.csv", metrics}, {"transactions.csv", out.Transactions}, {"blocks.csv", blocks}, {"minutes.csv", minutes}, {"events.csv", out.Events}} {
			if err = writeCSV(z, file.name, file.rows); err != nil {
				break
			}
		}
		if err == nil {
			file, e := z.Create("report.json")
			err = e
			if err == nil {
				err = json.NewEncoder(file).Encode(out)
			}
		}
		if err == nil {
			file, e := z.Create("report.html")
			err = e
			if err == nil {
				err = resultHTML.Execute(file, out)
			}
		}
		closeErr := z.Close()
		if err == nil {
			err = closeErr
		}
	default:
		encoder := json.NewEncoder(&data)
		encoder.SetIndent("", "  ")
		err = encoder.Encode(out)
	}
	if err != nil {
		fail(w, 500, err)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	disposition := "attachment"
	if format == "html" {
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", disposition+`; filename="pfap-`+out.Scope+`-report.`+extension+`"`)
	_, _ = w.Write(data.Bytes())
}
