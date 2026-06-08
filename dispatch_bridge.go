package main

// dispatch_bridge.go — thin adapters that wire the dispatch.go lifecycle
// commands to the already-ported engines (generators, dynamics fix/repair,
// inject, describe). Kept separate so the bridge targets are obvious and a
// single line retargets each if a ported symbol is ever renamed.

// dispGen2 → the rich config-driven dynamics generator (gendynamic2.go).
func dispGen2(args []string) { gendynamic2Main(args) }

// dispGen1 → the legacy dynamics generator (gendynamic.go).
func dispGen1(args []string) { genDynMain(args) }

// dispDynFix → dynamic-name reconcile (fixdynamic.go). It takes the same
// target list run_dynamics_fix passed to stacks_fix_dynamic.py.
func dispDynFix(names []string) { fixDynamicMain(names) }

// dispDynRepair → structural dynamics repair (repairdynamic.go). The bash ran
// the repair lib per resolved dynamic file; repairDynamicMain handles the
// target/all resolution itself.
func dispDynRepair(names []string) { repairDynamicMain(names) }

// dispInjectFile applies inject/strip/inject_urls to ONE stack file by routing
// through the ported inject engine (inject.go cmdInject), which resolves the
// file + reads art.conf the same way the bash did.
//
//	action: "inject" | "strip" | "inject_urls"
//	mode  : "all" | "art" | "urls" | "desc"
func dispInjectFile(action, file, mode string) {
	switch action {
	case "inject_urls":
		cmdInject([]string{"inject", file, "urls"})
	default:
		cmdInject([]string{action, file, mode})
	}
}

// dispInjectAll runs inject/strip across the whole stacks dir (or one target)
// via the ported engine.
func dispInjectAll(action, target string) {
	cmdInject([]string{action, target})
}

// dispDescribeFile adds (or strips) service descriptions for ONE file via the
// ported describe engine (describe.go describeMain).
//
//	action: "" for inject-descriptions, "strip" to remove them.
func dispDescribeFile(action, file string) {
	if action == "strip" {
		describeMain([]string{"strip", file})
		return
	}
	describeMain([]string{file})
}

// dispInjectDynamicFile applies dynamic-file art via the ported engine.
func dispInjectDynamicFile(action, file, dynDir string) {
	runInjectDynamic([]string{action, file, dynDir})
}
