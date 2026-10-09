package main

// config 子命令（PRD-0001 切分 2 / D4）：init/list/get/set/unset——把
// 「这台机器跟哪个 LLM 说话」沉淀为 YAML 配置文件，并履行「为什么生效
// 的是这个值」的诚实义务（命中路径与被遮蔽文件必须可见）。设计要点：
//   - list 人类可读走 stderr（anchor list 同构先例）、--headless JSON 走
//     stdout（run --headless 同规）——双通道均有既有先例，章程零修订
//    （R1 #4）。
//   - set 只做单键校验（复用切分 1 的 validate helper，validate.go）；
//     跨键完整性（decision 缺 url/model）留 run/verify 汇聚点——set 在
//     这里成功、在 run 报 D5 完整性文案，绝不复制第二条完整性语义。
//   - 写回语义（R2 #4）：现有文件走与 Load 同一严格解码，损坏即报错退
//     出 1 且文件内容逐字节不变——绝不宽松解码后写回（静默丢用户其它键
//     恰是本设计最忌讳的事）。实际改写走 yaml.Node 原位编辑：未触碰键
//     的顺序与注释在往返中存活。
//   - 密钥安全五道闸（D8）：显示恒掩码；文件 0600、目录 0700；project
//     作用域含密钥幂等追加 .gitignore；manifest 红线不动（文件层职责）；
//     argv/history/ps 通道用 `-` stdin 读值堵住——init 的 `-` 在向导开
//     始前前置读取（R2 #2 钉死：读至首个换行，无换行读至 EOF，九问行
//     协议从剩余 stdin 继续），set 无向导、`-` 即整读 stdin。
// 退出码只用 0/1（D4）。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/ByronFinn/PromptOpt/internal/config"
	"gopkg.in/yaml.v3"
)

// configStdin 是 config 子命令的 stdin 注入点（默认 os.Stdin，仿
// mcpCommand 的 In 注入）：单测以 strings.Reader 逐行喂答案，不依赖真
// 实终端。
var configStdin io.Reader = os.Stdin

// maskText 是 api_key/judge_api_key 的恒掩码（D8①）：list/get 永远显示
// 它，不做部分回显——单一常量在 config 包（Web 设置页共用同一常量）。
const maskText = config.MaskText

// configKeyOrder 是 22 个文件键的稳定展示/写出序——单一真源在 config
// 包（按 File 字段序反射派生，config.KeyOrder）；此处别名供既有调用点
// 与测试引用，TestConfigKeyOrderMatchesRegistry 继续钉住与登记表的集
// 合相等（新增文件键忘登记即测试红）。
var configKeyOrder = config.KeyOrder()

// D5 缺参指路文案（PRD-0001 D5，切分 2 钉死）：报错职责是一次性给全全
// 部合法出路——run 三途径（flag/env/配置文件+config init）、verify 四
// 途径（加 manifest 快照）；decision 缺参附 config 键指引（set 不拦跨
// 键完整性，这里就是完整性报错的落点）。退出码保持 1；文案快照钉在
// TestD5MissingParamGuidance。
var (
	errRunBaseURLRequired = errors.New("--base-url is required. 可通过三种途径配置（任选其一）：\n" +
		"  1. 命令行 flag：promptopt run --base-url http://localhost:8080/v1 ...\n" +
		"  2. 环境变量：export PROMPTOPT_BASE_URL=http://localhost:8080/v1\n" +
		"  3. 配置文件：promptopt config init（或手写 promptopt.yaml）")
	errRunModelRequired = errors.New("--model is required. 可通过三种途径配置（任选其一）：\n" +
		"  1. 命令行 flag：promptopt run --model <model-name> ...\n" +
		"  2. 环境变量：export PROMPTOPT_MODEL=<model-name>\n" +
		"  3. 配置文件：promptopt config init（或手写 promptopt.yaml）")
	errRunDecisionRequired = errors.New("--judge-backend decision requires --judge-decision-url and --judge-decision-model. 可通过三种途径配置（任选其一）：\n" +
		"  1. 命令行 flag：promptopt run --judge-decision-url http://decision-svc:9000 --judge-decision-model <model>\n" +
		"  2. 环境变量：export PROMPTOPT_JUDGE_DECISION_URL=... PROMPTOPT_JUDGE_DECISION_MODEL=...\n" +
		"  3. 配置文件：promptopt config set judge_decision_url <url> 与 judge_decision_model <model>")
	errVerifyBaseURLRequired = errors.New("--base-url is required. 可通过四种途径配置（任选其一）：\n" +
		"  1. 命令行 flag：promptopt verify <run_id> --base-url http://localhost:8080/v1\n" +
		"  2. 环境变量：export PROMPTOPT_BASE_URL=http://localhost:8080/v1\n" +
		"  3. 配置文件：promptopt config init（或手写 promptopt.yaml）\n" +
		"  4. run manifest 快照：manifest.json 含 base_url 时自动复现现场")
	errVerifyModelRequired = errors.New("--model is required. 可通过四种途径配置（任选其一）：\n" +
		"  1. 命令行 flag：promptopt verify <run_id> --model <model-name>\n" +
		"  2. 环境变量：export PROMPTOPT_MODEL=<model-name>\n" +
		"  3. 配置文件：promptopt config init（或手写 promptopt.yaml）\n" +
		"  4. run manifest 快照：manifest.json 含 model 时自动复现现场")
	errVerifyDecisionRequired = errors.New("--judge-backend decision 需要 --judge-decision-url 与 --judge-decision-model. 可通过四种途径配置（任选其一）：\n" +
		"  1. 命令行 flag：promptopt verify <run_id> --judge-decision-url http://decision-svc:9000 --judge-decision-model <model>\n" +
		"  2. 环境变量：export PROMPTOPT_JUDGE_DECISION_URL=... PROMPTOPT_JUDGE_DECISION_MODEL=...\n" +
		"  3. 配置文件：promptopt config set judge_decision_url <url> 与 judge_decision_model <model>\n" +
		"  4. run manifest 快照：manifest.json 含 judge_decision_url/judge_decision_model 时自动复现现场")
)

const configUsage = `usage: promptopt config <subcommand> [flags]

subcommands:
  init    交互式九问向导生成配置文件：config init [--scope user|project] [--config <path>] [--force]
          （非 tty 从 stdin 逐行读答案：空行取默认、EOF 余下全默认；--api-key - 在向导前从 stdin 前置读一行）
  list    查看生效配置与逐键来源：config list [--headless]（人类可读走 stderr；--headless JSON 走 stdout）
  get     查看单键生效值与来源：config get <key>
  set     写入单键（仅单键校验）：config set <key> <value> [--scope user|project]；值为 - 时整读 stdin
  unset   删除单键（键不存在幂等成功）：config unset <key> [--scope user|project]

发现序：--config > PROMPTOPT_CONFIG > ./promptopt.yaml > 用户级 promptopt/config.yaml，
首个存在的文件命中即止、命中即严格解码（未知键/类型错/语法错硬错退出 1）。
密钥通道：printf '%s\n' "$KEY" | promptopt config set api_key -   # 避开 shell history/ps
`

// configCommand dispatches the config subcommand (PRD-0001 D4)：与
// anchor 二级子命令同构——help/-h/--help 走 stdout+0，未知子命令走
// stderr+usage+1。
func configCommand(args []string) int {
	if len(args) == 0 {
		os.Stderr.WriteString(configUsage)
		return exitFailure
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "init":
		return configInit(rest, configStdin)
	case "list":
		return configList(rest)
	case "get":
		return configGet(rest)
	case "set":
		return configSet(rest, configStdin)
	case "unset":
		return configUnset(rest)
	case "help", "-h", "--help":
		os.Stdout.WriteString(configUsage)
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "promptopt config: unknown subcommand %q\n\n", sub)
		os.Stderr.WriteString(configUsage)
		return exitFailure
	}
}

// --- init：九问向导 ----------------------------------------------------------

// lineReader 在同一个 bufio.Reader 上顺序读行：`-` 前置读取与九问行协
// 议共用一个 reader，前置读取绝不吞掉后续行（R2 #2）。空行 = 取默认；
// EOF = 余下全默认。
type lineReader struct{ br *bufio.Reader }

// next 返回一行（去尾部 \r\n）；ok=false 表示输入耗尽。EOF 前的残余文
// 本仍算一行答案（无换行读至 EOF）。
func (lr *lineReader) next() (string, bool) {
	s, err := lr.br.ReadString('\n')
	s = strings.TrimRight(s, "\r\n")
	if err != nil {
		if errors.Is(err, io.EOF) {
			return s, s != ""
		}
		return "", false
	}
	return s, true
}

// readSecretLine 读一行作为密钥值：读至首个换行，无换行则读至 EOF
// （R2 #2/D8⑤）。尾部 \r 为 CRLF 管道修剪，其余空白原样保留。
func readSecretLine(r *bufio.Reader) (string, error) {
	s, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

// stdinIsInteractive 报告 stdin 是否字符设备（tty）：决定覆盖确认的形
// 态——tty 走交互 y/N，非 tty 默认拒绝覆盖（R2 #4）。标准库 os.Stat 即
// 可判别，零新增依赖。
func stdinIsInteractive(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// configInit runs the nine-question wizard (D4 问题序)：base_url、
// model、api_key、provider、optimizer、spec_metrics、独立裁判（yes 追
// 问五子项）、裁判后端（decision 追问四子项）、timeout 与 rps。flag 显
// 式给出的问题不再追问（flagsFirst 惯例，纯 flag 一次性完成）。
func configInit(args []string, stdin io.Reader) int {
	fset := flag.NewFlagSet("config init", flag.ContinueOnError)
	fset.SetOutput(os.Stderr)
	scope := fset.String("scope", "user", "写入作用域：user（用户级 config.yaml）或 project（./promptopt.yaml）")
	cfgPath := fset.String("config", "", "配置文件落点（显式路径，优先于 --scope；支持 ~ 展开）")
	force := fset.Bool("force", false, "目标已存在时直接覆盖（非 tty 缺省拒绝覆盖退出 1）")
	// 预置答案面：flag 名 → 文件键。显式给出的问题不再追问。
	vals := map[string]*string{}
	for _, name := range []string{
		"base-url", "model", "api-key", "provider", "optimizer", "spec-metrics",
		"timeout", "rps",
		"judge-provider", "judge-base-url", "judge-model", "judge-api-key", "judge-max-tokens",
		"judge-backend", "judge-decision-url", "judge-decision-model",
		"judge-decision-confidence", "judge-decision-diag-below",
	} {
		vals[name] = fset.String(name, "", "预置答案（向导不再追问该问题）")
	}
	if err := fset.Parse(flagsFirst(fset, args)); err != nil {
		return parseExitCode(err)
	}
	if fset.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "promptopt config init: unexpected argument %q\n", fset.Arg(0))
		return exitFailure
	}
	explicit := map[string]bool{}
	fset.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	target, err := writeTarget(*cfgPath, *scope)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt config init: %v\n", err)
		return exitFailure
	}

	// 单一 bufio.Reader 承载全部 stdin 消费：`-` 前置读取（按问题序
	// api_key → judge_api_key）→ 覆盖确认 → 九问行协议（R2 #2 顺序钉死）。
	br := bufio.NewReader(stdin)
	for _, key := range []string{"api_key", "judge_api_key"} {
		name := strings.ReplaceAll(key, "_", "-")
		if explicit[name] && *vals[name] == "-" {
			v, err := readSecretLine(br)
			if err != nil {
				fmt.Fprintf(os.Stderr, "promptopt config init: 读取 stdin 密钥失败: %v\n", err)
				return exitFailure
			}
			if v == "" {
				fmt.Fprintf(os.Stderr, "promptopt config init: --%s - 读到空 stdin\n", name)
				return exitFailure
			}
			*vals[name] = v
		}
	}

	// 覆盖保护（R2 #4）：目标已存在 → 严格 Load 读出、stderr 列出将被
	// 覆盖的已有键；非 tty 无 --force 默认拒绝（静默覆盖手工键 = 静默丢
	// 用户意图）；tty 走 y/N 交互确认；目标损坏时同样要求 --force。
	if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() {
		existing, err := config.Load(target)
		switch {
		case err != nil:
			if !*force {
				fmt.Fprintf(os.Stderr, "promptopt config init: 目标 %s 无法解析（%v）——确认覆盖请加 --force\n", target, err)
				return exitFailure
			}
			fmt.Fprintf(os.Stderr, "promptopt config init: 目标 %s 无法解析（%v）——--force 已给定，覆盖\n", target, err)
		default:
			if covered := coveredKeys(existing); len(covered) > 0 {
				fmt.Fprintf(os.Stderr, "promptopt config init: 目标 %s 已存在，将覆盖以下已有键：%s\n",
					target, strings.Join(covered, ", "))
				if !*force {
					if !stdinIsInteractive(stdin) {
						fmt.Fprintln(os.Stderr, "promptopt config init: 非 tty 环境默认拒绝覆盖；确认覆盖请加 --force")
						return exitFailure
					}
					fmt.Fprint(os.Stderr, "确认覆盖？[y/N]: ")
					line, err := readSecretLine(br)
					if err != nil {
						fmt.Fprintf(os.Stderr, "promptopt config init: 读取确认失败: %v\n", err)
						return exitFailure
					}
					switch strings.ToLower(strings.TrimSpace(line)) {
					case "y", "yes":
					default:
						fmt.Fprintln(os.Stderr, "promptopt config init: 已取消（未写入）")
						return exitFailure
					}
				}
			}
		}
	}

	// 九问行协议收集答案（flag 已给的问题不追问；校验复用 set 的
	// applySetKey——同一套单键校验语义）。
	interactive := stdinIsInteractive(stdin)
	lr := &lineReader{br: br}
	file, err := runWizard(lr, vals, explicit, interactive)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt config init: %v\n", err)
		return exitFailure
	}
	n, err := writeConfigFile(target, file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt config init: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(os.Stderr, "promptopt config init: 已写入 %s（%d 键，文件 0600/目录 0700）\n", target, n)
	if *scope == "project" {
		ensureGitignore(file)
	}
	return exitOK
}

// runWizard collects the nine answers in D4's question order and builds
// the File. 空行取默认（键缺位走链上回落）；EOF 余下全默认（必填项在
// tty 下重问，非 tty 直接落默认——文件缺 base_url 时 run 的 D5 指路文
// 案会把用户领回来）。
func runWizard(lr *lineReader, vals map[string]*string, explicit map[string]bool, interactive bool) (*config.File, error) {
	file := &config.File{}
	setAnswer := func(key, value string) error {
		if value == "" {
			return nil // 空行 = 取默认
		}
		if value == "-" {
			return fmt.Errorf("%s: 应答行的 - 不是合法值；密钥请用 --api-key - 通道在向导前从 stdin 读取", key)
		}
		return applySetKey(file, key, value)
	}
	ask := func(question, key string, required bool) error {
		name := strings.ReplaceAll(key, "_", "-")
		if explicit[name] {
			return setAnswer(key, *vals[name])
		}
		for {
			fmt.Fprintf(os.Stderr, "%s: ", question)
			line, ok := lr.next()
			if !ok {
				if interactive && required {
					return fmt.Errorf("必填项 %s 未作答（EOF），向导取消（未写入）", key)
				}
				return nil // EOF = 余下全默认
			}
			if line == "" {
				if interactive && required {
					fmt.Fprintf(os.Stderr, "（%s 必填，不能为空）\n", key)
					continue
				}
				return nil // 空行取默认
			}
			return setAnswer(key, line)
		}
	}

	if err := ask("[1/9] 执行器 base_url（必填，如 http://localhost:8080/v1）", "base_url", true); err != nil {
		return nil, err
	}
	if err := ask("[2/9] 执行器 model（必填）", "model", true); err != nil {
		return nil, err
	}
	if err := ask("[3/9] api_key（可选，回车取默认 \"1\"；密钥建议 --api-key - 走 stdin）", "api_key", false); err != nil {
		return nil, err
	}
	if err := ask("[4/9] provider（openai|anthropic，回车默认 openai）", "provider", false); err != nil {
		return nil, err
	}
	if err := ask("[5/9] optimizer（gepa/protegi/miprov2/evoprompt/p1/auto，回车默认 gepa；仅零配置模式生效）", "optimizer", false); err != nil {
		return nil, err
	}
	if err := ask("[6/9] spec_metrics（可选，逗号分隔；回车 = LLM 自选）", "spec_metrics", false); err != nil {
		return nil, err
	}

	// [7/9] 独立裁判 LLM（默认跳过——research 0001「可选（默认回退执行
	// 器）」）：任一 judge flag 显式给出即视为 yes，子问题仍按各自 flag
	// 显式性跳过。
	judge := false
	for _, name := range []string{"judge-provider", "judge-base-url", "judge-model", "judge-api-key", "judge-max-tokens"} {
		if explicit[name] {
			judge = true
		}
	}
	if !judge {
		fmt.Fprintf(os.Stderr, "[7/9] 独立裁判 LLM？（yes 配置；回车跳过——默认回落执行器）: ")
		if line, ok := lr.next(); ok {
			switch strings.ToLower(strings.TrimSpace(line)) {
			case "":
			case "y", "yes", "是":
				judge = true
			case "n", "no", "否":
			default:
				return nil, fmt.Errorf("独立裁判问题请回答 yes 或 no，got %q", line)
			}
		}
	}
	if judge {
		if err := ask("  [7a] judge provider（openai|anthropic；回车 = 同执行器）", "judge_provider", false); err != nil {
			return nil, err
		}
		if err := ask("  [7b] judge base_url（回车 = 同执行器 base_url）", "judge_base_url", false); err != nil {
			return nil, err
		}
		if err := ask("  [7c] judge model（回车 = 同执行器 model）", "judge_model", false); err != nil {
			return nil, err
		}
		if err := ask("  [7d] judge api_key（回车 = 同执行器 api_key 链）", "judge_api_key", false); err != nil {
			return nil, err
		}
		if err := ask("  [7e] judge max_tokens（回车 = 回落 --max-tokens）", "judge_max_tokens", false); err != nil {
			return nil, err
		}
	}

	// [8/9] 裁判后端：decision 追问 url/model 必填 + 两阈值可选。
	backend := ""
	if explicit["judge-backend"] {
		backend = *vals["judge-backend"]
		if backend != "" {
			if err := applySetKey(file, "judge_backend", backend); err != nil {
				return nil, err
			}
		}
	} else {
		fmt.Fprintf(os.Stderr, "[8/9] 裁判后端（llm|decision；回车默认 llm）: ")
		if line, ok := lr.next(); ok && strings.TrimSpace(line) != "" {
			backend = strings.TrimSpace(line)
			if err := applySetKey(file, "judge_backend", backend); err != nil {
				return nil, err
			}
		}
	}
	if backend == "decision" {
		if err := ask("  [8a] judge_decision_url（decision 必填）", "judge_decision_url", true); err != nil {
			return nil, err
		}
		if err := ask("  [8b] judge_decision_model（decision 必填）", "judge_decision_model", true); err != nil {
			return nil, err
		}
		if err := ask("  [8c] judge_decision_confidence（回车 = eval 默认 0.5）", "judge_decision_confidence", false); err != nil {
			return nil, err
		}
		if err := ask("  [8d] judge_decision_diag_below（回车 = eval 默认 0.6）", "judge_decision_diag_below", false); err != nil {
			return nil, err
		}
	}

	if err := ask("[9/9] timeout（Go 时长如 90s 或裸秒 300；回车默认 180s）", "timeout", false); err != nil {
		return nil, err
	}
	if err := ask("[9/9] rps（每秒请求数，0 = 不限；回车默认 0）", "rps", false); err != nil {
		return nil, err
	}
	return file, nil
}

// --- list / get --------------------------------------------------------------

// configKeyDefault 已上收 config.DefaultValue（与 Web 设置页同一实现，
// 展示语义不可能分叉）。

// effectiveConfig 在「无 run flag」语境（list/get）下解析逐键生效值：
// env > 文件 > 默认——实现单一真源在 config.EffectiveRows，来源判定与
// 解析链共用同一登记表（config.DeriveSources），list 与 Web 设置页
//（serve 回落路径）同源标注口径，不可能撒谎。
func effectiveConfig(disc config.Discovery) []config.SnapshotKey {
	return config.EffectiveRows(disc)
}

// configList prints the effective config: provenance first (hit path +
// shadowed files, R1 #10), then one row per key. 人类可读走 stderr；
// --headless JSON 走 stdout（anchor list / run --headless 同构先例）。
func configList(args []string) int {
	fset := flag.NewFlagSet("config list", flag.ContinueOnError)
	fset.SetOutput(os.Stderr)
	headless := fset.Bool("headless", false, "stdout 仅输出 JSON（与 run --headless 同规）")
	if err := fset.Parse(flagsFirst(fset, args)); err != nil {
		return parseExitCode(err)
	}
	if fset.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "promptopt config list: unexpected argument %q\n", fset.Arg(0))
		return exitFailure
	}
	disc, err := config.Discover("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt config list: %v\n", err)
		return exitFailure
	}
	keys := effectiveConfig(disc)
	if *headless {
		type keyJSON struct {
			Value  any    `json:"value"`
			Source string `json:"source"`
			Flag   string `json:"flag"`
		}
		out := struct {
			ConfigPath string             `json:"config_path"`
			Shadowed   []string           `json:"shadowed"`
			Warning    string             `json:"warning,omitempty"`
			Keys       map[string]keyJSON `json:"keys"`
		}{ConfigPath: disc.Path, Shadowed: disc.Shadowed, Warning: disc.Warning, Keys: map[string]keyJSON{}}
		if out.Shadowed == nil {
			out.Shadowed = []string{}
		}
		for _, k := range keys {
			out.Keys[k.Key] = keyJSON{Value: k.Value, Source: string(k.Source), Flag: k.Flag}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(os.Stderr, "promptopt config list: encode: %v\n", err)
			return exitFailure
		}
		return exitOK
	}
	// 人类可读 → stderr（anchor list 同构）。
	if disc.Warning != "" {
		fmt.Fprintln(os.Stderr, "promptopt config list: "+disc.Warning)
	}
	if disc.Path == "" {
		fmt.Fprintln(os.Stderr, "# config: （未发现配置文件——以下为 env 与默认值）")
	} else {
		fmt.Fprintf(os.Stderr, "# config: %s\n", disc.Path)
	}
	for _, s := range disc.Shadowed {
		fmt.Fprintf(os.Stderr, "# shadowed: %s（被上述文件遮蔽，不参与生效）\n", s)
	}
	w := tabwriter.NewWriter(os.Stderr, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "键\t值\t来源\tflag")
	for _, k := range keys {
		v := k.Value
		if s, ok := v.(string); ok && s == "" {
			v = "-"
		}
		fmt.Fprintf(w, "%s\t%v\t%s\t%s\n", k.Key, v, k.Source, k.Flag)
	}
	w.Flush()
	fmt.Fprintln(os.Stderr, "# 空值以 - 显示：base_url/model 必填未配；judge_* 空 = 回落执行器；judge_backend 空 = llm；spec_metrics 空 = LLM 自选")
	return exitOK
}

// configGet prints one key's effective value + source + (file 来源时) 命
// 中路径；未知键报错列全键集（D4）。
func configGet(args []string) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		os.Stdout.WriteString(configUsage)
		return exitOK
	}
	if len(args) != 1 {
		fmt.Fprintf(os.Stderr, "promptopt config get: 用法：config get <key>\n合法键：%s\n",
			strings.Join(configKeyOrder, ", "))
		return exitFailure
	}
	key := args[0]
	if !slices.Contains(configKeyOrder, key) {
		fmt.Fprintf(os.Stderr, "promptopt config get: 未知键 %q（合法键：%s）\n", key, strings.Join(configKeyOrder, ", "))
		return exitFailure
	}
	disc, err := config.Discover("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt config get: %v\n", err)
		return exitFailure
	}
	for _, k := range effectiveConfig(disc) {
		if k.Key != key {
			continue
		}
		note := ""
		if k.Source == config.SourceFile && disc.Path != "" {
			note = "，文件 " + disc.Path
		}
		fmt.Fprintf(os.Stderr, "%s = %v（来源=%s%s，flag %s）\n",
			key, k.Value, k.Source, note, k.Flag)
		return exitOK
	}
	return exitOK
}

// --- set / unset -------------------------------------------------------------

// applySetKey 解析并校验单键文本值，写入 File 的对应字段。config set 与
// init 共用；校验复用切分 1 的 helper（validate.go）——同一套语义，绝不
// 出现第二条校验。只做单键校验：跨键完整性（decision 缺 url/model）留
// run/verify 汇聚点。
func applySetKey(f *config.File, key, value string) error {
	fail := func(format string, a ...any) error {
		return fmt.Errorf("config set %s: %s", key, fmt.Sprintf(format, a...))
	}
	switch key {
	case "provider":
		if err := validateProvider(value); err != nil {
			return fail("%v", err)
		}
		f.Provider = value
	case "base_url", "model", "api_key", "out",
		"judge_base_url", "judge_model", "judge_api_key",
		"judge_decision_url", "judge_decision_model":
		// 无单键文法可校验的恒等键：URL 形状/模型名由 run/verify 汇聚点
		// 的必填与连通性检查兜底，这里照抄原值。
		switch key {
		case "base_url":
			f.BaseURL = value
		case "model":
			f.Model = value
		case "api_key":
			f.APIKey = value
		case "out":
			f.Out = value
		case "judge_base_url":
			f.JudgeBaseURL = value
		case "judge_model":
			f.JudgeModel = value
		case "judge_api_key":
			f.JudgeAPIKey = value
		case "judge_decision_url":
			f.JudgeDecisionURL = value
		case "judge_decision_model":
			f.JudgeDecisionModel = value
		}
	case "judge_provider":
		if err := validateJudgeProvider(value); err != nil {
			return fail("%v", err)
		}
		f.JudgeProvider = value
	case "max_tokens":
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return fail("必须是正整数，got %q", value)
		}
		f.MaxTokens = n
	case "judge_max_tokens":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return fail("必须是非负整数（0 = 回落 --max-tokens），got %q", value)
		}
		f.JudgeMaxTokens = n
	case "timeout":
		if _, err := validateTimeout(value); err != nil {
			return fail("%v", err)
		}
		f.Timeout = config.TimeoutValue(value)
	case "rps":
		v, err := strconv.ParseFloat(value, 64)
		if err != nil || v < 0 {
			return fail("必须是非负数字（0 = 不限速），got %q", value)
		}
		f.RPS = v
	case "extra_body":
		m, err := parseExtraBody(value)
		if err != nil {
			return fail("%v", err)
		}
		f.ExtraBody = m
	case "judge_backend":
		if err := validateJudgeBackend(value); err != nil {
			return fail("%v", err)
		}
		f.JudgeBackend = value
	case "judge_decision_confidence", "judge_decision_diag_below":
		v, err := strconv.ParseFloat(value, 64)
		if err != nil || v < 0 || v > 1 {
			return fail("必须在 [0, 1] 内，got %q", value)
		}
		if key == "judge_decision_confidence" {
			f.JudgeDecisionConfidence = v
		} else {
			f.JudgeDecisionDiagBelow = v
		}
	case "optimizer":
		if err := validateOptimizer(value); err != nil {
			return fail("%v", err)
		}
		f.Optimizer = value
	case "evo_variant":
		if value != "ga" && value != "de" {
			return fail("--evo-variant must be ga or de, got %q", value)
		}
		f.EvoVariant = value
	case "spec_metrics":
		list := splitSpecMetrics(value)
		if err := validateMetricsList(list); err != nil {
			return fail("%v", err)
		}
		f.SpecMetrics = list
	default:
		return fmt.Errorf("config set: 未知键 %q（合法键：%s）", key, strings.Join(configKeyOrder, ", "))
	}
	return nil
}

// splitSpecMetrics splits a comma-separated spec-metrics answer; 空项保
// 留原样交 validateMetricsList 报错（与 flag 侧同一准入）。
func splitSpecMetrics(v string) []string {
	out := make([]string, 0, strings.Count(v, ",")+1)
	for part := range strings.SplitSeq(v, ",") {
		out = append(out, strings.TrimSpace(part))
	}
	return out
}

// writeTarget resolves the write-side landing path: 显式 --config 路径
// （~ 展开，与发现层 1 同规则）优先；--scope project 写
// ./promptopt.yaml；缺省 user 写发现层 4 的用户级路径（D4）。
func writeTarget(explicit, scope string) (string, error) {
	if explicit != "" {
		return config.ExpandHome(explicit), nil
	}
	switch scope {
	case "project":
		return config.ProjectFileName, nil
	case "user":
		return config.UserLevelPath()
	default:
		return "", fmt.Errorf("--scope 必须是 user 或 project，got %q", scope)
	}
}

// loadExistingForWrite 以严格解码读取现有目标文件（R2 #4 写回语义）：
// 损坏即报错（调用方退出 1、文件保持逐字节不变），绝不宽松解码后写回；
// 文件不存在返回 nil（首次写入）。
func loadExistingForWrite(target string) (*config.File, error) {
	if _, err := os.Stat(target); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return config.Load(target)
}

// configSet writes one key: 仅单键校验（applySetKey）+ 严格解码写回
// （R2 #4）+ 密钥 `-` stdin 通道（D8⑤：set 无向导，`-` 即整读 stdin）。
func configSet(args []string, stdin io.Reader) int {
	fset := flag.NewFlagSet("config set", flag.ContinueOnError)
	fset.SetOutput(os.Stderr)
	scope := fset.String("scope", "user", "写入作用域：user（用户级 config.yaml）或 project（./promptopt.yaml）")
	if err := fset.Parse(flagsFirst(fset, args)); err != nil {
		return parseExitCode(err)
	}
	if fset.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "promptopt config set: 用法：config set <key> <value> [--scope user|project]")
		return exitFailure
	}
	key, value := fset.Arg(0), fset.Arg(1)
	if !slices.Contains(configKeyOrder, key) {
		fmt.Fprintf(os.Stderr, "promptopt config set: 未知键 %q（合法键：%s）\n", key, strings.Join(configKeyOrder, ", "))
		return exitFailure
	}
	if value == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "promptopt config set: 读取 stdin 失败: %v\n", err)
			return exitFailure
		}
		value = strings.TrimRight(string(b), "\r\n")
	} else if value == "" {
		fmt.Fprintf(os.Stderr, "promptopt config set: %s 的值不能为空（删除键请用 config unset）\n", key)
		return exitFailure
	}
	target, err := writeTarget("", *scope)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt config set: %v\n", err)
		return exitFailure
	}
	existing, err := loadExistingForWrite(target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt config set: %v（文件保持不变）\n", err)
		return exitFailure
	}
	file := config.File{}
	if existing != nil {
		file = *existing
	}
	if err := applySetKey(&file, key, value); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt config set: %v\n", err)
		return exitFailure
	}
	if err := writeConfigKey(target, key, value); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt config set: %v\n", err)
		return exitFailure
	}
	shown := value
	if config.SecretKey(key) {
		shown = maskText
	}
	fmt.Fprintf(os.Stderr, "promptopt config set: %s = %s（%s）\n", key, shown, target)
	if *scope == "project" && config.SecretKey(key) {
		ensureGitignore(nil)
	}
	return exitOK
}

// configUnset removes one key; 键不存在 = 幂等成功（D4）。写回语义同
// set（严格解码，损坏即错、文件不变）。
func configUnset(args []string) int {
	fset := flag.NewFlagSet("config unset", flag.ContinueOnError)
	fset.SetOutput(os.Stderr)
	scope := fset.String("scope", "user", "写入作用域：user（用户级 config.yaml）或 project（./promptopt.yaml）")
	if err := fset.Parse(flagsFirst(fset, args)); err != nil {
		return parseExitCode(err)
	}
	if fset.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "promptopt config unset: 用法：config unset <key> [--scope user|project]")
		return exitFailure
	}
	key := fset.Arg(0)
	if !slices.Contains(configKeyOrder, key) {
		fmt.Fprintf(os.Stderr, "promptopt config unset: 未知键 %q（合法键：%s）\n", key, strings.Join(configKeyOrder, ", "))
		return exitFailure
	}
	target, err := writeTarget("", *scope)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt config unset: %v\n", err)
		return exitFailure
	}
	if _, err := loadExistingForWrite(target); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt config unset: %v（文件保持不变）\n", err)
		return exitFailure
	}
	removed, err := editConfigNode(target, func(root *yaml.Node) (bool, error) {
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == key {
				root.Content = slices.Delete(root.Content, i, i+2)
				return true, nil
			}
		}
		return false, nil // 幂等：键不在，文件不动
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt config unset: %v\n", err)
		return exitFailure
	}
	if removed {
		fmt.Fprintf(os.Stderr, "promptopt config unset: 已删除 %s（%s）\n", key, target)
	} else {
		fmt.Fprintf(os.Stderr, "promptopt config unset: %s 不存在（%s），无需删除\n", key, target)
	}
	return exitOK
}

// --- 写侧文件操作（D8② 权限闸 + yaml.Node 原位改写）--------------------------

// atomicWrite writes data with 0600 permissions (D8②)，父目录按 0700 建
// 立；同目录临时文件 + rename 保证崩溃不留半截配置。已存在目录不被
// MkdirAll 改权（project 作用域绝不 chmod 用户项目根）。
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".promptopt-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// coveredKeys lists an existing file's non-zero keys — init 覆盖保护要
// 列出的「将被覆盖的已有键」（R2 #4）。键值读取走 config.File.Value
//（单一实现，Web 设置页共用）。
func coveredKeys(f *config.File) []string {
	var out []string
	for _, key := range configKeyOrder {
		if _, set := f.Value(key); set {
			out = append(out, key)
		}
	}
	return out
}

// writeConfigFile 把 file 的非零键按 configKeyOrder 序写成全新 YAML
// （init 路径：文件 0600、目录 0700），返回写出的键数。
func writeConfigFile(target string, file *config.File) (int, error) {
	root := &yaml.Node{Kind: yaml.MappingNode}
	for _, key := range configKeyOrder {
		v, set := file.Value(key)
		if !set {
			continue
		}
		node, err := typedValueNode(key, v)
		if err != nil {
			return 0, err
		}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: key}, node)
	}
	data, err := yaml.Marshal(root)
	if err != nil {
		return 0, err
	}
	if err := atomicWrite(target, data); err != nil {
		return 0, err
	}
	return len(root.Content) / 2, nil
}

// typedValueNode builds the value node for one key from its typed Go
// value（标量 Style 0 交给 emitter 按需加引号；map/序列经 marshal 往返
// 成节点）。
func typedValueNode(key string, v any) (*yaml.Node, error) {
	scalar := func(s string) *yaml.Node {
		return &yaml.Node{Kind: yaml.ScalarNode, Value: s}
	}
	switch t := v.(type) {
	case string:
		return scalar(t), nil
	case int:
		return scalar(strconv.Itoa(t)), nil
	case float64:
		return scalar(strconv.FormatFloat(t, 'g', -1, 64)), nil
	case map[string]any, []string:
		return rawYAMLNode(v)
	default:
		_ = key
		return nil, fmt.Errorf("键 %s 的值类型无法写出：%T", key, v)
	}
}

// rawYAMLNode marshals v to YAML and re-parses it as a node（map/序列值
// 的最短正确路径——手写映射节点遍历不值得）。
func rawYAMLNode(v any) (*yaml.Node, error) {
	b, err := yaml.Marshal(v)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0], nil
	}
	return &doc, nil
}

// writeConfigKey 以 Node 原位编辑写入单键（set 路径）：目标文件的语法
// 已由 loadExistingForWrite 严格校验，这里只增改目标键的值节点——其余
// 键的顺序与注释在往返中存活。标量 Style 0 交给 emitter 按需加引号。
func writeConfigKey(target, key, value string) error {
	_, err := editConfigNode(target, func(root *yaml.Node) (bool, error) {
		node, err := rawValueNode(key, value)
		if err != nil {
			return false, err
		}
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == key {
				root.Content[i+1] = node
				return true, nil
			}
		}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: key}, node)
		return true, nil
	})
	return err
}

// rawValueNode builds the value node from the validated raw text value:
// 标量直接落 Style 0 节点（解码端 string 目标取原始字面量、数值目标自
// 然解析，见 internal/config 的解码语义），extra_body/spec_metrics 两
// 个结构键经 marshal 往返。
func rawValueNode(key, value string) (*yaml.Node, error) {
	switch key {
	case "extra_body":
		m, err := parseExtraBody(value)
		if err != nil {
			return nil, err
		}
		return rawYAMLNode(m)
	case "spec_metrics":
		return rawYAMLNode(splitSpecMetrics(value))
	default:
		return &yaml.Node{Kind: yaml.ScalarNode, Value: value}, nil
	}
}

// editConfigNode loads target's YAML as an editable node tree, applies
// mutate, and atomically writes the result back when mutate reports a
// change. 目标不存在时从空映射起步（set 的首次写入路径）；unset 对不存
// 在的键/文件报告无改动、文件保持不动（幂等）。
func editConfigNode(target string, mutate func(root *yaml.Node) (bool, error)) (bool, error) {
	b, err := os.ReadFile(target)
	var root *yaml.Node
	switch {
	case err == nil:
		var doc yaml.Node
		if len(bytes.TrimSpace(b)) > 0 {
			if err := yaml.Unmarshal(b, &doc); err != nil {
				return false, fmt.Errorf("配置文件 %s: %w", target, err)
			}
		}
		switch {
		case doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 &&
			doc.Content[0].Kind == yaml.MappingNode:
			root = doc.Content[0]
		default:
			// 空文件或 null/标量根：新建映射（严格 Load 已把序列根等真
			// 损坏挡在前面）。
			root = &yaml.Node{Kind: yaml.MappingNode}
		}
	case errors.Is(err, fs.ErrNotExist):
		root = &yaml.Node{Kind: yaml.MappingNode}
	default:
		return false, err
	}
	changed, err := mutate(root)
	if err != nil || !changed {
		return changed, err
	}
	data, err := yaml.Marshal(root)
	if err != nil {
		return true, err
	}
	return true, atomicWrite(target, data)
}

// ensureGitignore is the D8③ gate：project 作用域结果含密钥时，./.gitignore
// 已存在且无 promptopt.yaml 条目 → 幂等追加并 stderr 说明一行；不存在
// → 打印指引不代建（.gitignore 的存在与否是用户的版本管理决策）。file
// 为 nil 时（config set 路径）密钥的存在已由调用方判定。
func ensureGitignore(file *config.File) {
	if file != nil && file.APIKey == "" && file.JudgeAPIKey == "" {
		return // 无密钥不设闸
	}
	const entry = config.ProjectFileName
	const gitignore = ".gitignore"
	b, err := os.ReadFile(gitignore)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "promptopt config: 提示：./%s 不存在——项目级 %s 含密钥，建议将其加入 %s 以免密钥入库\n", gitignore, entry, gitignore)
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt config: 读取 .gitignore 失败（%v）——请手工确认 %s 不入库\n", err, entry)
		return
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if strings.TrimSpace(line) == entry {
			return // 已有条目：幂等，不重复追加
		}
	}
	var out bytes.Buffer
	out.Write(b)
	if len(b) > 0 && b[len(b)-1] != '\n' {
		out.WriteByte('\n')
	}
	out.WriteString(entry + "\n")
	if err := os.WriteFile(gitignore, out.Bytes(), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt config: 追加 .gitignore 失败（%v）——请手工把 %s 加入版本库忽略清单\n", err, entry)
		return
	}
	fmt.Fprintf(os.Stderr, "promptopt config: 已把 %s 追加进 ./%s（项目级配置含密钥，防止密钥入库）\n", entry, gitignore)
}
