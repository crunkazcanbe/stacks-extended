package lib

// buildwizard.go — the centered, in-a-box build wizard. A faithful Go/bubbletea
// re-creation of the Python run_build_wizard(): ONE small box in the middle of
// the screen, and EVERYTHING happens inside it — the progress bar, every
// question, and the registry image search (a filterable list with "/" + A-Z
// jump, right in the box). Nothing takes over the whole screen.
//
// It reuses every scaffold helper from build.go (buildSvc / buildDBBlock /
// buildInject / buildDetectDB / buildFindExisting / buildNextIP / searchAll …),
// so only the UI is new. On finish it prints the same BUILD_OK / BUILD_START
// markers cmdBuild does, so the menu consumes it identically.

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── styles (match the menu's palette) ────────────────────────────────────────
var (
	bwBorder   = lipgloss.NewStyle().Foreground(lipgloss.Color("135"))
	bwTitle    = lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Bold(true)
	bwAccent   = lipgloss.NewStyle().Foreground(lipgloss.Color("81"))
	bwNormal   = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	bwSel      = lipgloss.NewStyle().Foreground(lipgloss.Color("16")).Background(lipgloss.Color("75"))
	bwDim      = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	bwBar      = lipgloss.NewStyle().Foreground(lipgloss.Color("81"))
	bwYellow   = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	bwGreen    = lipgloss.NewStyle().Foreground(lipgloss.Color("82"))
	bwRed      = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
)

// ── phases ───────────────────────────────────────────────────────────────────
const (
	bwPhStack     = "stack"     // pick target stack
	bwPhImage     = "image"     // type an image OR a keyword
	bwPhSearch    = "search"    // results list (filterable)
	bwPhName      = "name"      // container name
	bwPhIP        = "ip"        // service IP
	bwPhPort      = "port"      // service port
	bwPhDBAsk     = "db_ask"    // needs a database? (y/n)
	bwPhDBType    = "db_type"   // which db?
	bwPhDBPick    = "db_pick"   // use existing or new
	bwPhDBName    = "db_name"   // new db: name
	bwPhDBIP      = "db_ip"     // new db: ip
	bwPhDBPass    = "db_pass"   // new db: password
	bwPhDBDb      = "db_db"     // new db: database name
	bwPhRedisAsk  = "redis_ask" // needs redis? (y/n)
	bwPhCompAsk   = "comp_ask"  // needs a companion container? (y/n)
	bwPhCompImage = "comp_img"  // companion: image name / keyword
	bwPhCompName  = "comp_name" // companion: container name
	bwPhScaffold  = "scaffold"  // building…
	bwPhVolAsk    = "vol_ask"   // add a manual volume? (y/n)
	bwPhVolPick   = "vol_pick"  // multi-select which services get a volume
	bwPhVolEach   = "vol_each"  // type a (possibly different) volume per chosen service
	bwPhNetAsk    = "net_ask"   // add a network manually to services? (y/n)
	bwPhNetName   = "net_name"  // type the network name
	bwPhNetPick   = "net_pick"  // multi-select which services get the network
	bwPhStart     = "start"     // start it now? (y/n)
	bwPhStartMode = "start_md"  // service-only or whole stack
	bwPhDone      = "done"
)

// bwSearchDoneMsg carries async registry-search results back into the model.
type bwSearchDoneMsg struct {
	term    string
	results []searchResult
}

// bwScaffoldDoneMsg signals the build/inject finished.
type bwScaffoldDoneMsg struct {
	ok  bool
	msg string
}

type bwModel struct {
	w, h int
	cfg  buildConf

	phase string
	pct   int

	// collected answers
	targetStack string
	image       string
	svcName     string
	svcIP       string
	svcPort     string
	db          *buildDBRec
	redis       *buildDBRec

	// scratch for the current question
	prompt  string
	input   string   // text buffer (input phases)
	items   []string // list phases (raw display labels)
	values  []string // parallel: the underlying value for each item
	cursor  int
	scroll  int
	flt     string // "/" filter text
	filting bool   // currently typing into the filter
	yesSel  int    // yes/no cursor (0 = yes)

	// search state
	searching bool
	results   []searchResult
	searchFor string // "main" or "companion" — where a picked image lands

	// companion container
	compImage string
	compName  string

	// manual extras
	manualNet string       // manual network name to add to chosen services
	selected  map[int]bool // multi-select state (service picker)
	volQueue  []string     // services chosen to get a manual volume (typed per-service)
	volIdx    int          // which service in volQueue we're typing a volume for

	// db scratch
	dbType    string
	dbExisting []buildDBRec

	// outcome
	status   string
	statusOK bool
	quit     bool

	// printed-on-exit markers
	buildOK    string // svc name if built
	startSpec  string // "" | "<stack>" | "<stack> <svc>"
}

// ── launch ───────────────────────────────────────────────────────────────────

// cmdBuildBox runs the in-box wizard. argv mirrors cmdBuild: [image svc stack] /
// [svc stack] / [svc]. A 3-arg call (image preset) is handed straight to the
// classic non-interactive path's scaffold via the wizard's prefilled state.
func cmdBuildBox(argv []string) {
	var args []string
	for _, a := range argv {
		if a == "--progress" || a == "--box" || strings.HasPrefix(a, "/tmp/") {
			continue
		}
		args = append(args, a)
	}
	m := &bwModel{cfg: buildLoadConf(), svcPort: "8080", phase: bwPhStack, pct: 0}
	if len(args) >= 3 {
		m.image, m.svcName, m.targetStack = args[0], args[1], args[2]
	} else if len(args) == 2 {
		m.svcName, m.targetStack = args[0], args[1]
	} else if len(args) == 1 {
		m.svcName = args[0]
	}
	m.beginPhaseChain()

	p := tea.NewProgram(m, tea.WithAltScreen())
	res, err := p.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "build wizard:", err)
		os.Exit(1)
	}
	fm, _ := res.(*bwModel)
	if fm == nil || fm.buildOK == "" {
		return
	}
	fmt.Printf("\n\033[1;32m✨ BUILD COMPLETE: %s\033[0m\n", fm.buildOK)
	fmt.Printf("BUILD_OK:%s\n", fm.buildOK)
	if fm.startSpec != "" {
		fmt.Printf("BUILD_START:%s\n", fm.startSpec)
	}
}

// beginPhaseChain seeds the first un-answered question given any preset args.
func (m *bwModel) beginPhaseChain() {
	if m.targetStack != "" {
		m.phase = bwPhImage
	}
	if m.image != "" {
		m.phase = bwPhName
	}
	m.enter(m.phase)
}

func (m *bwModel) Init() tea.Cmd { return nil }

// ── phase setup ──────────────────────────────────────────────────────────────

// enter prepares the scratch state + prompt for a phase.
func (m *bwModel) enter(phase string) tea.Cmd {
	m.phase = phase
	m.input = ""
	m.flt = ""
	m.filting = false
	m.cursor = 0
	m.scroll = 0
	m.yesSel = 1
	m.items = nil
	m.values = nil

	switch phase {
	case bwPhStack:
		m.pct = 5
		var names []string
		for _, f := range buildListYML(stacksDir()) {
			if !strings.HasPrefix(f, "db_") {
				names = append(names, strings.TrimSuffix(f, ".yml"))
			}
		}
		sort.Strings(names)
		for _, n := range names {
			m.items = append(m.items, n)
			m.values = append(m.values, n)
		}
		m.prompt = "Select target stack:"
	case bwPhImage:
		m.pct = 10
		m.prompt = "Image name (or a keyword to search):"
		m.input = ""
	case bwPhSearch:
		m.pct = 15
		m.prompt = "Pick an image  ( / filter · A-Z jump · ENTER pick · ESC back )"
	case bwPhName:
		m.pct = 25
		m.prompt = "Container name:"
		m.input = m.svcName
	case bwPhIP:
		m.pct = 30
		m.prompt = "Service IP (192.168.1.x):"
		m.input = buildNextIP()
	case bwPhPort:
		m.pct = 35
		m.prompt = "Service port:"
		m.input = "8080"
	case bwPhDBAsk:
		m.pct = 45
		m.prompt = "Does this service need a database?"
	case bwPhDBType:
		m.pct = 48
		m.prompt = "Which database?"
		for _, t := range []string{"postgres", "mysql", "redis", "mongo"} {
			m.items = append(m.items, t)
			m.values = append(m.values, t)
		}
	case bwPhDBPick:
		m.pct = 50
		m.prompt = "Use an existing " + m.dbType + " or create new?"
		m.dbExisting = buildFindExisting(m.dbType)
		for _, e := range m.dbExisting {
			ipStr := "no IP"
			if e.IP != "" {
				ipStr = e.IP + ":" + e.Port
			}
			m.items = append(m.items, fmt.Sprintf("USE  %s  (%s)  [%s]", e.Name, ipStr, e.Stack))
			m.values = append(m.values, "use:"+e.Name)
		}
		m.items = append(m.items, "NEW  Create new "+m.dbType+" container")
		m.values = append(m.values, "new")
	case bwPhDBName:
		m.pct = 52
		m.prompt = m.dbType + " container name:"
		m.input = m.svcName + "-" + m.dbType
	case bwPhDBIP:
		m.pct = 53
		m.prompt = m.dbType + " IP address:"
		m.input = buildNextIP()
	case bwPhDBPass:
		m.pct = 54
		m.prompt = m.dbType + " password:"
		m.input = "changeme"
	case bwPhDBDb:
		m.pct = 55
		m.prompt = "Database name:"
		m.input = strings.ReplaceAll(m.svcName, "-", "_")
	case bwPhRedisAsk:
		m.pct = 60
		m.prompt = "Does this service need Redis?"
	case bwPhCompAsk:
		m.pct = 64
		m.prompt = "Does this service need a companion container?"
	case bwPhCompImage:
		m.pct = 66
		m.prompt = "Companion image (or a keyword to search):"
		m.input = ""
	case bwPhCompName:
		m.pct = 68
		m.prompt = "Companion container name:"
		m.input = m.svcName + "-worker"
	case bwPhVolAsk:
		m.pct = 86
		m.prompt = "Add a manual volume to one or more services? (auto bind-mount already added)"
	case bwPhVolPick:
		m.pct = 87
		m.prompt = "Select services to add a volume to  ( SPACE toggle · ENTER next )"
		m.selected = map[int]bool{}
		for _, s := range bwStackServices(m.targetStack) {
			m.items = append(m.items, s)
			m.values = append(m.values, s)
		}
	case bwPhVolEach:
		m.pct = 88
		cur := ""
		if m.volIdx < len(m.volQueue) {
			cur = m.volQueue[m.volIdx]
		}
		m.prompt = fmt.Sprintf("Volume for %s  (host/path:/ctr  OR  name:/ctr · blank = skip):", cur)
		m.input = ""
	case bwPhNetAsk:
		m.pct = 90
		m.prompt = "Add a network manually to services in this stack?"
	case bwPhNetName:
		m.pct = 92
		m.prompt = "Network name to add:"
		m.input = ""
	case bwPhNetPick:
		m.pct = 94
		m.prompt = "Select services to add '" + m.manualNet + "' to  ( SPACE toggle · ENTER apply )"
		m.selected = map[int]bool{}
		for _, s := range bwStackServices(m.targetStack) {
			m.items = append(m.items, s)
			m.values = append(m.values, s)
		}
	case bwPhScaffold:
		m.pct = 70
		m.prompt = "Building compose scaffold…"
		m.status = "Working…"
		return m.scaffoldCmd()
	case bwPhStart:
		m.pct = 100
		m.prompt = fmt.Sprintf("Start %s now?", m.svcName)
	case bwPhStartMode:
		m.pct = 100
		m.prompt = "Start (s)ervice only or (w)hole stack?"
		m.items = []string{"Service only", "Whole stack"}
		m.values = []string{"svc", "stack"}
	}
	return nil
}

// kind returns the widget kind for the current phase.
func (m *bwModel) kind() string {
	switch m.phase {
	case bwPhStack, bwPhSearch, bwPhDBType, bwPhDBPick, bwPhStartMode:
		return "list"
	case bwPhNetPick, bwPhVolPick:
		return "multi"
	case bwPhDBAsk, bwPhRedisAsk, bwPhCompAsk, bwPhVolAsk, bwPhNetAsk, bwPhStart:
		return "yesno"
	case bwPhScaffold:
		return "status"
	default:
		return "input"
	}
}

// ── update ───────────────────────────────────────────────────────────────────

func (m *bwModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil
	case bwSearchDoneMsg:
		m.searching = false
		m.results = msg.results
		m.rebuildSearchItems()
		if m.phase != bwPhSearch {
			return m, m.enter(bwPhSearch)
		}
		return m, nil
	case bwScaffoldDoneMsg:
		m.status = msg.msg
		m.statusOK = msg.ok
		if msg.ok {
			m.buildOK = m.svcName // mark built so cmdBuildBox prints BUILD_OK on exit
			return m, m.enter(bwPhVolAsk)
		}
		// failure → show message then quit
		m.quit = true
		return m, tea.Quit
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *bwModel) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.Type == tea.KeyCtrlC {
		m.quit = true
		return m, tea.Quit
	}
	switch m.kind() {
	case "input":
		return m.handleInputKey(k)
	case "list":
		return m.handleListKey(k)
	case "yesno":
		return m.handleYesNoKey(k)
	case "multi":
		return m.handleMultiKey(k)
	case "status":
		return m, nil // ignore keys while working
	}
	return m, nil
}

func (m *bwModel) handleMultiKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.selected == nil {
		m.selected = map[int]bool{}
	}
	switch k.Type {
	case tea.KeyUp:
		if m.cursor > 0 {
			m.cursor--
		}
	case tea.KeyDown:
		if m.cursor < len(m.items)-1 {
			m.cursor++
		}
	case tea.KeySpace:
		m.selected[m.cursor] = !m.selected[m.cursor]
	case tea.KeyEnter:
		return m.confirmMulti()
	case tea.KeyEsc:
		// skip this step
		if m.phase == bwPhVolPick {
			return m, m.enter(bwPhNetAsk)
		}
		return m, m.enter(bwPhStart)
	case tea.KeyRunes:
		if len(k.Runes) == 1 && k.Runes[0] == ' ' {
			m.selected[m.cursor] = !m.selected[m.cursor]
		}
	}
	m.fixScroll()
	return m, nil
}

func (m *bwModel) confirmMulti() (tea.Model, tea.Cmd) {
	var chosen []string
	for i, v := range m.values {
		if m.selected[i] {
			chosen = append(chosen, v)
		}
	}
	switch m.phase {
	case bwPhVolPick:
		if len(chosen) == 0 {
			return m, m.enter(bwPhNetAsk)
		}
		m.volQueue = chosen
		m.volIdx = 0
		return m, m.enter(bwPhVolEach)
	case bwPhNetPick:
		if len(chosen) > 0 {
			bwApplyNetToServices(m.targetStack, m.manualNet, chosen)
		}
		return m, m.enter(bwPhNetAsk) // loop: offer to add another network
	}
	return m, nil
}

func (m *bwModel) handleInputKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.Type {
	case tea.KeyEnter:
		return m.submitInput(strings.TrimSpace(m.input))
	case tea.KeyEsc:
		// ESC on the very first question cancels; otherwise no-op
		if m.phase == bwPhStack || (m.phase == bwPhImage && m.targetStack == "") {
			m.quit = true
			return m, tea.Quit
		}
		return m, nil
	case tea.KeyBackspace:
		if len(m.input) > 0 {
			m.input = m.input[:len(m.input)-1]
		}
	case tea.KeyRunes, tea.KeySpace:
		m.input += string(k.Runes)
		if k.Type == tea.KeySpace {
			m.input += " "
		}
	}
	return m, nil
}

func (m *bwModel) submitInput(val string) (tea.Model, tea.Cmd) {
	switch m.phase {
	case bwPhImage:
		if val == "" {
			return m, nil
		}
		// If it already looks like a concrete image ref (has a registry/namespace
		// slash or an explicit :tag), use it directly — no search, exactly like
		// the Python: "type the correct name and it uses it without searching."
		if strings.Contains(val, "/") || strings.Contains(val, ":") {
			m.image = val
			return m, m.enter(bwPhName)
		}
		// Otherwise treat it as a keyword and search the registries in-box.
		m.searchFor = "main"
		m.enter(bwPhSearch)
		m.searching = true
		m.results = nil
		m.items = nil
		return m, m.searchCmd(val)
	case bwPhCompImage:
		if val == "" {
			return m, m.enter(bwPhScaffold) // skip companion if left blank
		}
		if strings.Contains(val, "/") || strings.Contains(val, ":") {
			m.compImage = val
			return m, m.enter(bwPhCompName)
		}
		m.searchFor = "companion"
		m.enter(bwPhSearch)
		m.searching = true
		m.results = nil
		m.items = nil
		return m, m.searchCmd(val)
	case bwPhCompName:
		m.compName = orDef(val, m.svcName+"-worker")
		return m, m.enter(bwPhScaffold)
	case bwPhName:
		if val != "" {
			m.svcName = val
		}
		return m, m.enter(bwPhIP)
	case bwPhIP:
		m.svcIP = orDef(val, buildNextIP())
		return m, m.enter(bwPhPort)
	case bwPhPort:
		m.svcPort = orDef(val, "8080")
		// NOTE: deliberately NOT auto-inspecting the image here — buildDetectDB can
		// docker-pull (up to 60s) which would freeze the box. The DB question is a
		// plain yes/no → type pick instead.
		return m, m.enter(bwPhDBAsk)
	case bwPhDBName:
		m.db = &buildDBRec{Type: m.dbType, Name: orDef(val, m.svcName+"-"+m.dbType),
			New: true, Net: m.svcName + "_net"}
		return m, m.enter(bwPhDBIP)
	case bwPhDBIP:
		m.db.IP = orDef(val, buildNextIP())
		m.db.Port = defPort(m.dbType)
		m.db.Image = defImage(m.dbType)
		return m, m.enter(bwPhDBPass)
	case bwPhDBPass:
		m.db.Password = orDef(val, "changeme")
		return m, m.enter(bwPhDBDb)
	case bwPhDBDb:
		m.db.DBName = orDef(val, strings.ReplaceAll(m.svcName, "-", "_"))
		// stack: default db_0
		m.db.Stack = "db_0.yml"
		return m, m.enter(bwPhRedisAsk)
	case bwPhVolEach:
		// apply this (possibly unique) volume to the current service, then move to
		// the next selected service; blank = skip that one.
		if val != "" && m.volIdx < len(m.volQueue) {
			bwApplyVolToService(m.targetStack, m.volQueue[m.volIdx], val)
		}
		m.volIdx++
		if m.volIdx < len(m.volQueue) {
			return m, m.enter(bwPhVolEach)
		}
		return m, m.enter(bwPhVolAsk) // loop: offer another round of volumes
	case bwPhNetName:
		if val == "" {
			return m, m.enter(bwPhStart)
		}
		m.manualNet = val
		return m, m.enter(bwPhNetPick)
	}
	return m, nil
}

func (m *bwModel) handleListKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	// "/" filter typing mode (search + any list)
	if m.filting {
		switch k.Type {
		case tea.KeyEnter, tea.KeyEsc:
			m.filting = false
		case tea.KeyBackspace:
			if len(m.flt) > 0 {
				m.flt = m.flt[:len(m.flt)-1]
			}
			m.applyFilter()
		case tea.KeyRunes:
			m.flt += string(k.Runes)
			m.applyFilter()
		}
		return m, nil
	}
	switch k.Type {
	case tea.KeyUp:
		if m.cursor > 0 {
			m.cursor--
		}
	case tea.KeyDown:
		if m.cursor < len(m.items)-1 {
			m.cursor++
		}
	case tea.KeyEnter:
		return m.pickList()
	case tea.KeyEsc:
		if m.phase == bwPhSearch {
			if m.searchFor == "companion" {
				return m, m.enter(bwPhCompImage)
			}
			return m, m.enter(bwPhImage)
		}
		if m.phase == bwPhStack {
			m.quit = true
			return m, tea.Quit
		}
		return m, nil
	case tea.KeyRunes:
		r := k.Runes[0]
		if r == '/' {
			m.filting = true
			m.flt = ""
			return m, nil
		}
		// A-Z jump: go to first item starting with the letter
		lr := strings.ToLower(string(r))
		for i, it := range m.items {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(it)), lr) {
				m.cursor = i
				break
			}
		}
	}
	m.fixScroll()
	return m, nil
}

func (m *bwModel) pickList() (tea.Model, tea.Cmd) {
	if m.cursor < 0 || m.cursor >= len(m.values) {
		return m, nil
	}
	v := m.values[m.cursor]
	switch m.phase {
	case bwPhStack:
		m.targetStack = v
		return m, m.enter(bwPhImage)
	case bwPhSearch:
		if m.searchFor == "companion" {
			m.compImage = v
			return m, m.enter(bwPhCompName)
		}
		m.image = v // v = Pull ref
		return m, m.enter(bwPhName)
	case bwPhDBType:
		m.dbType = v
		return m, m.enter(bwPhDBPick)
	case bwPhDBPick:
		if v == "new" {
			return m, m.enter(bwPhDBName)
		}
		name := strings.TrimPrefix(v, "use:")
		for _, e := range m.dbExisting {
			if e.Name == name {
				m.db = &buildDBRec{Type: m.dbType, Name: e.Name, IP: e.IP, Port: e.Port,
					Stack: e.Stack, New: false, Net: m.svcName + "_net"}
				break
			}
		}
		return m, m.enter(bwPhRedisAsk)
	case bwPhStartMode:
		if v == "stack" {
			m.startSpec = m.targetStack
		} else {
			m.startSpec = m.targetStack + " " + m.svcName
		}
		m.quit = true
		return m, tea.Quit
	}
	return m, nil
}

func (m *bwModel) handleYesNoKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.Type {
	case tea.KeyLeft, tea.KeyRight:
		m.yesSel = 1 - m.yesSel
	case tea.KeyEnter:
		return m.pickYesNo(m.yesSel == 0)
	case tea.KeyEsc:
		return m.pickYesNo(false)
	case tea.KeyRunes:
		switch strings.ToLower(string(k.Runes)) {
		case "y":
			return m.pickYesNo(true)
		case "n":
			return m.pickYesNo(false)
		}
	}
	return m, nil
}

func (m *bwModel) pickYesNo(yes bool) (tea.Model, tea.Cmd) {
	switch m.phase {
	case bwPhDBAsk:
		if yes {
			if m.dbType != "" {
				return m, m.enter(bwPhDBPick)
			}
			return m, m.enter(bwPhDBType)
		}
		return m, m.enter(bwPhRedisAsk)
	case bwPhRedisAsk:
		if yes {
			m.redis = &buildDBRec{Type: "redis", Name: m.svcName + "-redis",
				IP: buildNextIP(), Port: "6379", Image: "redis:7-alpine",
				New: true, Net: m.svcName + "_net", Stack: "db_0.yml"}
		}
		return m, m.enter(bwPhCompAsk)
	case bwPhCompAsk:
		if yes {
			return m, m.enter(bwPhCompImage)
		}
		return m, m.enter(bwPhScaffold)
	case bwPhVolAsk:
		if yes {
			return m, m.enter(bwPhVolPick)
		}
		return m, m.enter(bwPhNetAsk)
	case bwPhNetAsk:
		if yes {
			return m, m.enter(bwPhNetName)
		}
		return m, m.enter(bwPhStart)
	case bwPhStart:
		if yes {
			return m, m.enter(bwPhStartMode)
		}
		m.startSpec = ""
		m.quit = true
		return m, tea.Quit
	}
	return m, nil
}

// ── async commands ───────────────────────────────────────────────────────────

func (m *bwModel) searchCmd(term string) tea.Cmd {
	return func() tea.Msg {
		var flat []searchResult
		for _, rows := range searchAll(term, 1, 100) {
			for _, r := range rows {
				if r.Err == "" && r.Pull != "" {
					flat = append(flat, r)
				}
			}
		}
		sort.Slice(flat, func(i, j int) bool { return flat[i].Pull < flat[j].Pull })
		return bwSearchDoneMsg{term: term, results: flat}
	}
}

func (m *bwModel) scaffoldCmd() tea.Cmd {
	// snapshot the values needed (m is a pointer; closure reads are fine here
	// since nothing mutates them before this runs)
	return func() tea.Msg {
		fpath := m.targetStack
		if !strings.HasSuffix(fpath, ".yml") {
			fpath += ".yml"
		}
		// count existing services for the #NN comment
		svcNum := buildCountServices(stacksDir()+"/"+fpath) + 1
		svcNet := m.svcName + "_net"
		block := buildSvc(m.svcName, m.image, m.svcIP, m.svcPort, m.cfg, svcNum, m.db, m.redis)
		if !buildInject(m.targetStack, block, svcNet, "") {
			return bwScaffoldDoneMsg{ok: false, msg: "✘ Could not inject " + m.svcName + " (already exists?)"}
		}
		if m.db != nil && m.db.New {
			dblk, dvol := buildDBBlock(m.db, m.svcName)
			buildInject(m.db.Stack, dblk, svcNet, dvol)
		}
		if m.redis != nil && m.redis.New {
			rblk, rvol := buildDBBlock(m.redis, m.svcName)
			buildInject(m.redis.Stack, rblk, svcNet, rvol)
		}
		if m.compImage != "" {
			cblk := buildSvc(m.compName, m.compImage, buildNextIP(), m.svcPort, m.cfg, svcNum+1, nil, nil)
			buildInject(m.targetStack, cblk, svcNet, "")
		}
		// Create the new external network so `stacks up` can attach to it (mirrors
		// the classic flow's final step). buildInject already declared it
		// external:true in the stack file.
		ni := exec.Command("docker", "network", "inspect", svcNet)
		ni.Env = dockerEnv()
		if ni.Run() != nil {
			nc := exec.Command("docker", "network", "create", svcNet)
			nc.Env = dockerEnv()
			nc.Run()
		}
		return bwScaffoldDoneMsg{ok: true, msg: "✔ Built " + m.svcName}
	}
}

// ── view ─────────────────────────────────────────────────────────────────────

func (m *bwModel) View() string {
	if m.w == 0 {
		return ""
	}
	pw := m.w - 4
	if pw > 74 {
		pw = 74
	}
	inner := pw - 4
	var b strings.Builder

	// title + progress bar
	titles := map[string]string{
		bwPhStack: "Stack", bwPhImage: "Image", bwPhSearch: "Image",
		bwPhName: "Name", bwPhIP: "IP", bwPhPort: "Port",
		bwPhDBAsk: "Database", bwPhDBType: "Database", bwPhDBPick: "Database",
		bwPhDBName: "Database", bwPhDBIP: "Database", bwPhDBPass: "Database", bwPhDBDb: "Database",
		bwPhRedisAsk: "Redis", bwPhScaffold: "Build", bwPhStart: "Start", bwPhStartMode: "Start",
		bwPhCompAsk: "Companion", bwPhCompImage: "Companion", bwPhCompName: "Companion",
	}
	b.WriteString(bwTitle.Render("Build New Service — "+titles[m.phase]) + "\n")
	bw := inner
	filled := bw * m.pct / 100
	if filled > bw {
		filled = bw
	}
	b.WriteString(bwBar.Render("["+strings.Repeat("█", filled)+strings.Repeat("░", bw-filled)+"] ") +
		bwYellow.Render(fmt.Sprintf("%d%%", m.pct)) + "\n\n")

	// prompt
	b.WriteString(bwAccent.Render(bwClip(m.prompt, inner)) + "\n")

	// body by widget kind
	switch m.kind() {
	case "input":
		b.WriteString("\n  " + bwNormal.Render("> "+m.input) + bwDim.Render("▏") + "\n")
		b.WriteString("\n" + bwDim.Render("ENTER confirm · ESC cancel"))
	case "yesno":
		yes, no := "  YES  ", "  NO   "
		if m.yesSel == 0 {
			yes = bwSel.Render(yes) + "   " + bwNormal.Render(no)
		} else {
			yes = bwNormal.Render(yes) + "   " + bwSel.Render(no)
		}
		b.WriteString("\n  " + yes + "\n")
		b.WriteString("\n" + bwDim.Render("←→ select · ENTER · y/n"))
	case "status":
		col := bwYellow
		if m.statusOK {
			col = bwGreen
		}
		b.WriteString("\n  " + col.Render(m.status))
	case "list":
		b.WriteString(m.renderList(inner))
	case "multi":
		b.WriteString(m.renderMulti(inner))
	}

	box := bwBorder.Render(bwBox(b.String(), pw))
	return lipgloss.Place(m.w, m.h, lipgloss.Center, lipgloss.Center, box)
}

func (m *bwModel) renderMulti(inner int) string {
	var b strings.Builder
	m.fixScroll()
	rows := 7
	end := m.scroll + rows
	if end > len(m.items) {
		end = len(m.items)
	}
	if len(m.items) == 0 {
		b.WriteString("\n  " + bwDim.Render("(no services found)"))
	}
	for i := m.scroll; i < end; i++ {
		box := "[ ]"
		if m.selected[i] {
			box = "[x]"
		}
		label := bwClip(m.items[i], inner-6)
		line := box + " " + label
		if i == m.cursor {
			b.WriteString(" " + bwSel.Render(" "+line) + "\n")
		} else {
			b.WriteString("   " + bwNormal.Render(line) + "\n")
		}
	}
	b.WriteString(bwDim.Render("↑↓ move · SPACE toggle · ENTER apply · ESC skip"))
	return b.String()
}

func (m *bwModel) renderList(inner int) string {
	var b strings.Builder
	if m.searching {
		return "\n  " + bwYellow.Render("⠿ searching registries…")
	}
	if m.filting || m.flt != "" {
		b.WriteString("  " + bwDim.Render("/") + bwNormal.Render(m.flt))
		if m.filting {
			b.WriteString(bwDim.Render("▏"))
		}
		b.WriteString("\n")
	}
	rows := 7
	end := m.scroll + rows
	if end > len(m.items) {
		end = len(m.items)
	}
	if len(m.items) == 0 {
		b.WriteString("\n  " + bwDim.Render("(no matches)"))
	}
	for i := m.scroll; i < end; i++ {
		label := bwClip(m.items[i], inner-3)
		if i == m.cursor {
			b.WriteString(" " + bwSel.Render(" ▶ "+label) + "\n")
		} else {
			b.WriteString("   " + bwNormal.Render(label) + "\n")
		}
	}
	b.WriteString(bwDim.Render("↑↓ move · / filter · A-Z jump · ENTER · ESC"))
	return b.String()
}

// ── search/filter helpers ────────────────────────────────────────────────────

func (m *bwModel) rebuildSearchItems() {
	m.items = nil
	m.values = nil
	for _, r := range m.results {
		label := r.Pull
		if r.Source != "" {
			label = fmt.Sprintf("%-44s %s", bwClip(r.Pull, 44), r.Source)
		}
		m.items = append(m.items, label)
		m.values = append(m.values, r.Pull)
	}
	m.cursor, m.scroll = 0, 0
}

// applyFilter narrows the displayed list by the "/" filter text. For the search
// phase it filters the full result set; for plain lists it filters in place.
func (m *bwModel) applyFilter() {
	f := strings.ToLower(m.flt)
	if m.phase == bwPhSearch {
		m.items = nil
		m.values = nil
		for _, r := range m.results {
			if f == "" || strings.Contains(strings.ToLower(r.Pull), f) ||
				strings.Contains(strings.ToLower(r.Desc), f) {
				label := fmt.Sprintf("%-44s %s", bwClip(r.Pull, 44), r.Source)
				m.items = append(m.items, label)
				m.values = append(m.values, r.Pull)
			}
		}
		m.cursor, m.scroll = 0, 0
	}
}

func (m *bwModel) fixScroll() {
	rows := 7
	if m.cursor < m.scroll {
		m.scroll = m.cursor
	}
	if m.cursor >= m.scroll+rows {
		m.scroll = m.cursor - rows + 1
	}
}

// ── small utils ──────────────────────────────────────────────────────────────

func bwBox(content string, pw int) string {
	// wrap content into a fixed-width bordered block via lipgloss
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Width(pw - 2).
		Padding(0, 1).
		Render(content)
}

func bwClip(s string, n int) string {
	if n < 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

func orDef(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func defPort(t string) string {
	switch t {
	case "mysql":
		return "3306"
	case "redis":
		return "6379"
	case "mongo":
		return "27017"
	default:
		return "5432"
	}
}

func defImage(t string) string {
	switch t {
	case "mysql":
		return "mariadb:10.11"
	case "redis":
		return "redis:7-alpine"
	case "mongo":
		return "mongo:7"
	default:
		return "postgres:16-alpine"
	}
}

// ── manual network/volume helpers (edit a stack file in place) ───────────────

// bwStackPath resolves a stack name to its .yml path.
func bwStackPath(stack string) string {
	p := stacksDir() + "/" + stack
	if !strings.HasSuffix(p, ".yml") {
		p += ".yml"
	}
	return p
}

// bwStackServices lists the service keys (container blocks) in a stack file.
func bwStackServices(stack string) []string {
	data, err := os.ReadFile(bwStackPath(stack))
	if err != nil {
		return nil
	}
	var out []string
	inServices := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "services:") {
			inServices = true
			continue
		}
		if len(line) > 0 && line[0] != ' ' && line[0] != '#' {
			inServices = false
		}
		if !inServices {
			continue
		}
		if len(line) >= 3 && line[0] == ' ' && line[1] == ' ' && line[2] != ' ' && line[2] != '#' {
			t := strings.TrimSpace(line)
			if strings.HasSuffix(t, ":") && !strings.HasPrefix(t, "x-") {
				out = append(out, strings.TrimSuffix(t, ":"))
			}
		}
	}
	return out
}

// bwServiceBlockRange returns [start,end) line indices of a service's block.
func bwServiceBlockRange(lines []string, service string) (int, int) {
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "  ") && (len(l) < 3 || l[2] != ' ') && strings.TrimSpace(l) == service+":" {
			start = i
			break
		}
	}
	if start < 0 {
		return -1, -1
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		l := lines[i]
		if l == "" {
			continue
		}
		if l[0] != ' ' && l[0] != '#' { // top-level key
			end = i
			break
		}
		if len(l) >= 3 && l[0] == ' ' && l[1] == ' ' && l[2] != ' ' && strings.HasSuffix(strings.TrimSpace(l), ":") {
			end = i
			break
		}
	}
	return start, end
}

// bwApplyVolToService adds a volume mount line to one service's volumes: block.
func bwApplyVolToService(stack, service, vol string) {
	path := bwStackPath(stack)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	s, e := bwServiceBlockRange(lines, service)
	if s < 0 {
		return
	}
	entry := `      - "` + vol + `"`
	volIdx := -1
	for i := s + 1; i < e; i++ {
		if strings.TrimRight(lines[i], " ") == "    volumes:" {
			volIdx = i
			break
		}
	}
	if volIdx >= 0 {
		lines = append(lines[:volIdx+1], append([]string{entry}, lines[volIdx+1:]...)...)
	} else {
		block := []string{"    volumes:", entry}
		lines = append(lines[:s+1], append(block, lines[s+1:]...)...)
	}
	os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644)
}

// bwApplyNetToServices adds a network to each chosen service + registers it
// top-level as an external network (mirrors the build convention).
func bwApplyNetToServices(stack, net string, services []string) {
	path := bwStackPath(stack)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	for _, svc := range services {
		s, e := bwServiceBlockRange(lines, svc)
		if s < 0 {
			continue
		}
		has := false
		for i := s; i < e; i++ {
			if strings.TrimSpace(lines[i]) == net+":" {
				has = true
				break
			}
		}
		if has {
			continue
		}
		add := []string{"      " + net + ":", "        priority: 500"}
		netIdx := -1
		for i := s + 1; i < e; i++ {
			if strings.TrimRight(lines[i], " ") == "    networks:" {
				netIdx = i
				break
			}
		}
		if netIdx >= 0 {
			lines = append(lines[:netIdx+1], append(add, lines[netIdx+1:]...)...)
		} else {
			block := append([]string{"    networks:"}, add...)
			lines = append(lines[:s+1], append(block, lines[s+1:]...)...)
		}
	}
	content := strings.Join(lines, "\n")
	// register top-level external network if not already declared
	if !regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(net) + `:`).MatchString(content) {
		reg := "  " + net + ": {name: " + net + ", external: true}"
		topRe := regexp.MustCompile(`(?m)^networks:[ \t]*$`)
		if loc := topRe.FindStringIndex(content); loc != nil {
			content = content[:loc[1]] + "\n" + reg + content[loc[1]:]
		} else {
			content += "\nnetworks:\n" + reg + "\n"
		}
	}
	os.WriteFile(path, []byte(content), 0644)
}

// buildCountServices counts service blocks in a compose file (for the #NN tag).
func buildCountServices(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	inServices := false
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "services:") {
			inServices = true
			continue
		}
		if len(line) > 0 && line[0] != ' ' && line[0] != '#' {
			inServices = false
		}
		if !inServices {
			continue
		}
		t := strings.TrimSpace(line)
		if len(line) >= 3 && line[0] == ' ' && line[1] == ' ' && line[2] != ' ' &&
			strings.HasSuffix(t, ":") && !strings.HasPrefix(t, "x-") {
			n++
		}
	}
	return n
}
