package main

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 判定“相同文件”的标准：内容 SHA-256 完全相同（同名/同路径/修改时间都不算）。
//
// 流程：
//   1. filepath.WalkDir 遍历两个目录，按 -group-by 指定的维度分组（只取元数据，不读内容）；
//   2. 只有两边都存在同一分组键的文件，才进入哈希阶段（能过滤掉绝大多数文件）；
//   3. 对候选文件流式计算 SHA-256，按哈希求交集，输出重复结果或执行删除。

func main() {
	var (
		target          string
		current         string
		deleteMode      string
		groupBy         string
		exts            string
		excludeNames    string
		reportUnmatched bool
		dryRun          bool
		yes             bool
		quiet           bool
		report          string
		maxReport       int
		minSize         int64
	)
	flag.StringVar(&target, "target", "", "必传：要与当前目录对比的目标目录路径")
	flag.StringVar(&current, "current", ".", "当前目录，默认为当前工作目录")
	flag.StringVar(&deleteMode, "delete-mode", "notdelete",
		"删除模式：notdelete(默认不删除) / current(重复时删除当前目录文件) / target(重复时删除目标目录文件)")
	flag.StringVar(&groupBy, "group-by", "size",
		"分组方式：size(默认，按文件大小分组，全量比对) / name(按文件名分组，只对同名文件做哈希比对，最快)")
	flag.StringVar(&exts, "ext", "", "只处理这些后缀的文件，逗号分隔（如 .mp4,.jpeg）；留空（默认）表示不限制")
	flag.StringVar(&excludeNames, "exclude-name", "", "跳过这些文件名，逗号分隔；留空（默认）表示不排除")
	flag.BoolVar(&reportUnmatched, "report-unmatched", true,
		"候选组内哈希不一致时也输出 DIFF 行（含双方 size 与 sha256），便于排查“看着一样但哈希不同”")
	flag.BoolVar(&dryRun, "dry-run", false, "试运行：只报告将要删除的文件，不真正删除")
	flag.BoolVar(&yes, "yes", false, "删除前不再交互确认")
	flag.BoolVar(&quiet, "quiet", false, "不打印进度")
	flag.StringVar(&report, "report", "", "报告输出文件(CSV)，为空则输出到标准输出")
	flag.IntVar(&maxReport, "max-report", 20, "每个重复组最多输出的配对行数，0 表示不限制")
	flag.Int64Var(&minSize, "min-size", 0, "忽略小于该字节数的文件")
	flag.Parse()

	if strings.TrimSpace(target) == "" {
		fmt.Fprintln(os.Stderr, "错误：必须使用 -target 指定目标目录")
		flag.Usage()
		os.Exit(2)
	}
	switch deleteMode {
	case "notdelete", "current", "target":
	default:
		fmt.Fprintf(os.Stderr, "错误：-delete-mode 只能是 notdelete/current/target，当前值：%s\n", deleteMode)
		os.Exit(2)
	}
	switch strings.ToLower(strings.TrimSpace(groupBy)) {
	case "size", "name":
	default:
		fmt.Fprintf(os.Stderr, "错误：-group-by 只能是 size/name，当前值：%s\n", groupBy)
		os.Exit(2)
	}

	cfg := config{
		current:         current,
		target:          target,
		deleteMode:      deleteMode,
		groupBy:         strings.ToLower(strings.TrimSpace(groupBy)),
		exts:            parseList(exts),
		excludeNames:    parseList(excludeNames),
		reportUnmatched: reportUnmatched,
		dryRun:          dryRun,
		quiet:           quiet,
		report:          report,
		maxReport:       maxReport,
		minSize:         minSize,
	}
	if err := run(cfg, yes); err != nil {
		fmt.Fprintln(os.Stderr, "执行失败:", err)
		os.Exit(1)
	}
}

// parseList 把逗号分隔的配置项解析成小写集合，空串返回 nil 表示不限制。
func parseList(s string) map[string]struct{} {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out := make(map[string]struct{})
	for _, item := range strings.Split(s, ",") {
		if v := strings.ToLower(strings.TrimSpace(item)); v != "" {
			out[v] = struct{}{}
		}
	}
	return out
}

type config struct {
	current         string
	target          string
	deleteMode      string
	groupBy         string // size | name
	exts            map[string]struct{}
	excludeNames    map[string]struct{}
	reportUnmatched bool
	dryRun          bool
	quiet           bool
	report          string
	maxReport       int
	minSize         int64
}

type hit struct {
	path string
	size int64
}

type stats struct {
	filesA, filesB int64
	skipped        int64
	groupsA        int64 // 当前目录分组键数量
	groupsB        int64 // 目标目录分组键数量
	candGroups     int64 // 两边都存在的分组键数量
	candFiles      int64 // 真正参与哈希的文件数量
	diffGroups     int64 // 候选组里哈希不一致的组数量
	pairs          int64
	dupA, dupB     int64
	dupBytes       int64
	delCnt         int64
	delBytes       int64
	errs           []string
}

func (s *stats) addErr(path string, err error) {
	if len(s.errs) < 200 {
		s.errs = append(s.errs, fmt.Sprintf("%s : %v", path, err))
	}
}

func run(cfg config, yes bool) error {
	start := time.Now()

	curAbs, err := filepath.Abs(cfg.current)
	if err != nil {
		return err
	}
	tgtAbs, err := filepath.Abs(cfg.target)
	if err != nil {
		return err
	}
	for _, p := range []string{curAbs, tgtAbs} {
		fi, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("目录不可访问 %s: %w", p, err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("不是目录: %s", p)
		}
	}
	if samePath(curAbs, tgtAbs) {
		return fmt.Errorf("当前目录与目标目录不能是同一个目录")
	}
	if rel, e := filepath.Rel(curAbs, tgtAbs); e == nil && !strings.HasPrefix(rel, "..") {
		fmt.Fprintln(os.Stderr, "警告：目标目录位于当前目录内部，会被重复扫描")
	}

	// 报告输出
	var repFile *os.File
	w := csv.NewWriter(os.Stdout)
	skip := map[string]struct{}{}
	if cfg.report != "" {
		absRep, _ := filepath.Abs(cfg.report)
		skip[pathKey(absRep)] = struct{}{}
		repFile, err = os.Create(absRep)
		if err != nil {
			return err
		}
		defer repFile.Close()
		w = csv.NewWriter(repFile)
	}
	defer w.Flush()
	w.Write([]string{"action", "size", "sha256", "current_path", "target_path", "note"})

	// 选择分组方式
	var collectFn collector = collectBySize
	switch cfg.groupBy {
	case "name":
		collectFn = collectByName
	}

	var st stats
	fmt.Fprintf(os.Stderr, "当前目录: %s\n目标目录: %s\n分组方式: %s\n删除模式: %s (dry-run=%v)\n\n",
		curAbs, tgtAbs, cfg.groupBy, cfg.deleteMode, cfg.dryRun)

	// 1) 遍历分组
	fmt.Fprintln(os.Stderr, "[1/3] 扫描当前目录 ...")
	mapA, err := collectFn(curAbs, cfg, skip, &st.filesA, &st)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "[2/3] 扫描目标目录 ...")
	mapB, err := collectFn(tgtAbs, cfg, skip, &st.filesB, &st)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "当前目录文件数: %d，目标目录文件数: %d\n", st.filesA, st.filesB)
	st.groupsA, st.groupsB = int64(len(mapA)), int64(len(mapB))
	fmt.Fprintf(os.Stderr, "分组键数量: 当前 %d 组，目标 %d 组\n\n", st.groupsA, st.groupsB)
	if noCommonKeys(mapA, mapB) {
		fmt.Fprintf(os.Stderr,
			"注意：%s 模式下两边分组键交集为 0，即没有任何可比对的文件，后续不会哈希任何文件。\n"+
				"      当前目录示例键: %v\n      目标目录示例键: %v\n",
			cfg.groupBy, sampleKeys(mapA, 5), sampleKeys(mapB, 5))
	}

	if cfg.deleteMode != "notdelete" && !cfg.dryRun && !yes {
		side := "当前目录"
		if cfg.deleteMode == "target" {
			side = "目标目录"
		}
		fmt.Fprintf(os.Stderr, "即将删除【%s】中与另一目录重复的文件。输入 yes 继续: ", side)
		var answer string
		fmt.Scanln(&answer)
		if strings.TrimSpace(strings.ToLower(answer)) != "yes" {
			return fmt.Errorf("已取消，未做任何删除")
		}
	}

	// 2) 按分组键求交集，只对候选文件计算 SHA-256
	fmt.Fprintln(os.Stderr, "[3/3] 计算候选文件 SHA-256 并比对 ...")
	lastLog := time.Now()
	for key, listA := range mapA {
		listB, ok := mapB[key]
		if !ok {
			continue
		}
		st.candGroups++
		st.candFiles += int64(len(listA)) + int64(len(listB))
		hashA := hashAll(listA, &st)
		hashB := hashAll(listB, &st)
		common := sortedCommon(hashA, hashB)

		// 候选组里哈希完全对不上：输出 DIFF 行，把双方的 size / sha256 都列出来
		if len(common) == 0 {
			st.diffGroups++
			if cfg.reportUnmatched {
				reportDiff(cfg, w, key, hashA, hashB)
			}
		}

		for _, sum := range common {
			as := hashA[sum]
			bs := hashB[sum]
			st.pairs += int64(len(as)) * int64(len(bs))
			st.dupA += int64(len(as))
			st.dupB += int64(len(bs))
			for i := range as {
				st.dupBytes += as[i].size
			}

			// 输出报告
			shown := 0
		loop:
			for _, a := range as {
				for _, b := range bs {
					if cfg.maxReport > 0 && shown >= cfg.maxReport {
						break loop
					}
					w.Write([]string{"SAME", fmt.Sprint(a.size), sum, a.path, b.path, ""})
					fmt.Fprintf(os.Stderr, "SAME: %s, %s\n", a.path, b.path)
					shown++
				}
			}
			if cfg.maxReport > 0 && shown == cfg.maxReport {
				w.Write([]string{"SAME_TRUNCATED", "", sum, "", "",
					fmt.Sprintf("分组键 %s 仅输出前 %d 条", key, cfg.maxReport)})
			}

			// 删除
			var victims []hit
			if cfg.deleteMode == "current" {
				victims = as
			} else if cfg.deleteMode == "target" {
				victims = bs
			}
			for _, v := range victims {
				delOne(&st, cfg, w, sum, v, keeperOf(cfg.deleteMode, as, bs))
			}
		}

		if !cfg.quiet && time.Since(lastLog) > 10*time.Second {
			lastLog = time.Now()
			fmt.Fprintf(os.Stderr, "  ... 已发现重复配对 %d\n", st.pairs)
		}
	}
	w.Flush()

	// 3) 汇总
	fmt.Fprintln(os.Stderr, "\n================= 汇总 =================")
	fmt.Fprintf(os.Stderr, "分组方式       : %s\n", cfg.groupBy)
	fmt.Fprintf(os.Stderr, "当前目录文件数 : %d\n", st.filesA)
	fmt.Fprintf(os.Stderr, "目标目录文件数 : %d\n", st.filesB)
	fmt.Fprintf(os.Stderr, "跳过非普通文件 : %d\n", st.skipped)
	fmt.Fprintf(os.Stderr, "分组键数量     : 当前 %d 组，目标 %d 组\n", st.groupsA, st.groupsB)
	fmt.Fprintf(os.Stderr, "候选分组(交集) : %d 组\n", st.candGroups)
	fmt.Fprintf(os.Stderr, "参与哈希文件数 : %d\n", st.candFiles)
	fmt.Fprintf(os.Stderr, "哈希不一致     : %d 组（同组但内容不同，详见 CSV 的 DIFF 行）\n", st.diffGroups)
	fmt.Fprintf(os.Stderr, "重复配对数     : %d\n", st.pairs)
	fmt.Fprintf(os.Stderr, "当前目录重复   : %d 个文件\n", st.dupA)
	fmt.Fprintf(os.Stderr, "目标目录重复   : %d 个文件\n", st.dupB)
	fmt.Fprintf(os.Stderr, "重复占用空间   : %s (按当前目录侧计)\n", humanSize(st.dupBytes))
	if cfg.deleteMode != "notdelete" {
		verb := "已删除"
		if cfg.dryRun {
			verb = "将删除"
		}
		fmt.Fprintf(os.Stderr, "%s文件     : %d 个，释放 %s\n", verb, st.delCnt, humanSize(st.delBytes))
	}
	fmt.Fprintf(os.Stderr, "耗时           : %s\n", time.Since(start).Round(time.Millisecond))
	if len(st.errs) > 0 {
		fmt.Fprintf(os.Stderr, "\n错误/警告 %d 条（最多显示 20 条）：\n", len(st.errs))
		n := len(st.errs)
		if n > 20 {
			n = 20
		}
		for i := 0; i < n; i++ {
			fmt.Fprintln(os.Stderr, "  -", st.errs[i])
		}
	}
	fmt.Fprintln(os.Stderr, "========================================")
	return nil
}

type collector func(root string, cfg config, skip map[string]struct{},
	counter *int64, st *stats) (map[string][]hit, error)

// collect 公共遍历逻辑：keyFn 决定按什么维度分组。
func collect(root string, cfg config, skip map[string]struct{}, counter *int64, st *stats,
	keyFn func(name string, info fs.FileInfo) string) (map[string][]hit, error) {

	out := make(map[string][]hit)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			st.addErr(path, err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if _, ok := skip[pathKey(path)]; ok {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			st.skipped++
			return nil
		}
		if _, ok := skip[pathKey(path)]; ok {
			return nil
		}
		name := d.Name()
		if len(cfg.excludeNames) > 0 {
			if _, ok := cfg.excludeNames[strings.ToLower(name)]; ok {
				return nil
			}
		}
		if len(cfg.exts) > 0 {
			if _, ok := cfg.exts[strings.ToLower(filepath.Ext(name))]; !ok {
				return nil
			}
		}
		info, err := d.Info()
		if err != nil {
			st.addErr(path, err)
			return nil
		}
		if info.Size() < cfg.minSize {
			return nil
		}

		key := keyFn(name, info)
		out[key] = append(out[key], hit{path: path, size: info.Size()})
		*counter++

		if cfg.groupBy == "name" && len(out[key]) == 2 {
			fmt.Fprintf(os.Stderr, "提示：同一文件名出现在多处 %s -> %v\n", key, pathsOf(out[key]))
		}
		if !cfg.quiet && *counter%200000 == 0 {
			fmt.Fprintf(os.Stderr, "  ... 已扫描 %d 个文件\n", *counter)
		}
		return nil
	})
	return out, err
}

// collectBySize 按文件大小分组（默认，最通用：能发现改名后的重复文件）。
func collectBySize(root string, cfg config, skip map[string]struct{},
	counter *int64, st *stats) (map[string][]hit, error) {
	return collect(root, cfg, skip, counter, st, func(name string, info fs.FileInfo) string {
		return strconv.FormatInt(info.Size(), 10)
	})
}

// collectByName 按文件名分组（只会比对两边同名的文件，速度快但发现不了改名重复）。
// 分组键统一转小写：Windows 文件系统不区分大小写，.MP4 与 .mp4 应视为同一个键。
func collectByName(root string, cfg config, skip map[string]struct{},
	counter *int64, st *stats) (map[string][]hit, error) {
	return collect(root, cfg, skip, counter, st, func(name string, info fs.FileInfo) string {
		return strings.ToLower(name)
	})
}

func pathsOf(hs []hit) []string {
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.path)
	}
	return out
}

// noCommonKeys 判断两边的分组键是否完全没有交集。
func noCommonKeys(a, b map[string][]hit) bool {
	small, large := a, b
	if len(small) > len(large) {
		small, large = large, small
	}
	for k := range small {
		if _, ok := large[k]; ok {
			return false
		}
	}
	return true
}

// sampleKeys 取 n 个分组键用于排查问题。
func sampleKeys(m map[string][]hit, n int) []string {
	keys := make([]string, 0, n)
	for k := range m {
		keys = append(keys, k)
		if len(keys) >= n {
			break
		}
	}
	sort.Strings(keys)
	return keys
}

// hashAll 计算一组文件的 SHA-256，返回 hash(hex) -> 文件列表。
func hashAll(list []hit, st *stats) map[string][]hit {
	out := make(map[string][]hit, len(list))
	buf := make([]byte, 1<<20)
	for _, h := range list {
		sum, err := sha256File(h.path, buf)
		if err != nil {
			st.addErr(h.path, err)
			continue
		}
		out[sum] = append(out[sum], h)
	}
	return out
}

func sha256File(path string, buf []byte) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type hashed struct {
	h   hit
	sum string
}

// flattenHashed 把 hash -> 文件列表 摊平并按路径排序，保证输出稳定。
func flattenHashed(m map[string][]hit) []hashed {
	out := make([]hashed, 0, len(m))
	for sum, hs := range m {
		for _, h := range hs {
			out = append(out, hashed{h: h, sum: sum})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].h.path < out[j].h.path })
	return out
}

// reportDiff 输出“分到同一组但内容不同”的文件，带双方 size 与 sha256。
func reportDiff(cfg config, w *csv.Writer, key string, hashA, hashB map[string][]hit) {
	fa, fb := flattenHashed(hashA), flattenHashed(hashB)
	n := len(fa)
	if len(fb) > n {
		n = len(fb)
	}
	for i := 0; i < n; i++ {
		if cfg.maxReport > 0 && i >= cfg.maxReport {
			w.Write([]string{"DIFF_TRUNCATED", "", "", "", "",
				fmt.Sprintf("分组键 %s 仅输出前 %d 条", key, cfg.maxReport)})
			return
		}
		size, sum, cur, tgt, note := "", "", "", "", ""
		if i < len(fa) {
			size, sum, cur = fmt.Sprint(fa[i].h.size), fa[i].sum, fa[i].h.path
		}
		if i < len(fb) {
			tgt = fb[i].h.path
			note = fmt.Sprintf("目标 size=%d sha256=%s", fb[i].h.size, fb[i].sum)
			if i >= len(fa) {
				size, sum = fmt.Sprint(fb[i].h.size), fb[i].sum
			}
		}
		w.Write([]string{"DIFF", size, sum, cur, tgt, note})
	}
}

// sortedCommon 返回两边哈希集合的交集（排序保证输出稳定）。
func sortedCommon(a, b map[string][]hit) []string {
	var keys []string
	for k := range a {
		if _, ok := b[k]; ok {
			keys = append(keys, k)
		}
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func keeperOf(mode string, as, bs []hit) hit {
	if mode == "current" {
		return bs[0]
	}
	return as[0]
}

func delOne(st *stats, cfg config, w *csv.Writer, sum string, victim, keeper hit) {
	cur, tgt := victim.path, keeper.path
	if cfg.deleteMode == "target" {
		cur, tgt = keeper.path, victim.path
	}
	action := "DELETE_" + strings.ToUpper(cfg.deleteMode)
	row := func(a string, note string) []string {
		return []string{a, fmt.Sprint(victim.size), sum, cur, tgt, note}
	}

	if cfg.dryRun {
		w.Write(row("DRYRUN_"+action, ""))
		st.delCnt++
		st.delBytes += victim.size
		return
	}
	if err := os.Remove(victim.path); err != nil {
		st.addErr(victim.path, err)
		w.Write(row("DELETE_FAILED", err.Error()))
		return
	}
	w.Write(row(action, ""))
	st.delCnt++
	st.delBytes += victim.size
}

func pathKey(p string) string {
	return filepath.Clean(p)
}

func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for i := n / unit; i >= unit; i /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
