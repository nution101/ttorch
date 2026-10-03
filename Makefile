VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG     := github.com/nution101/ttorch/internal/buildinfo
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)
PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64

.PHONY: test-gate build install test test-fast vet fmt fmtcheck lint dist clean

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/ttorch ./cmd/ttorch

# Local developer install: build into the user-owned home, link into PATH, lay content.
install:
	@mkdir -p $(HOME)/.ttorch/bin $(HOME)/.local/bin
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(HOME)/.ttorch/bin/ttorch ./cmd/ttorch
	ln -sf $(HOME)/.ttorch/bin/ttorch $(HOME)/.local/bin/ttorch
	$(HOME)/.ttorch/bin/ttorch install

# Every go test here runs under an explicit -timeout rather than go's 10m default, which the
# full internal/orchestrator run used to exceed on its own (669s in one process on the build
# host). Each value is several times the lane's measured run time there, so it fires on a hang,
# not on a busy machine. Override one for a slower machine, e.g. TEST_TIMEOUT=40m.
TEST_TIMEOUT      ?= 20m
TEST_FAST_TIMEOUT ?= 10m
TEST_GATE_TIMEOUT ?= 10m

# internal/orchestrator's tests drive real tmux windows, git repositories and validate runs,
# and spend most of their time waiting on those processes. They cannot use t.Parallel:
# nearly every one sets the environment (TTORCH_HOME, TTORCH_TMUX_SESSION) or swaps a
# package-level seam. So the package runs as ORCH_SHARDS go test processes at once, each with
# its own environment and, through TestMain, its own tmux server. ORCH_SHARDS=1 runs it in one.
ORCH_SHARDS ?= 4
ORCH_PKG    := ./internal/orchestrator/

# ORCH_SHARDS can come from the environment, and .ttorch/validate.sh inherits the environment of
# whatever runs the gate. A count the loop below cannot use must stop make rather than start no
# shard and pass, so orch_shards accepts exactly one of these words and nothing else: not 0, not
# a negative, not "4 -o 1", not "04".
ORCH_SHARD_COUNTS := 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30 31 32
orch_shards_check = $(if $(and $(filter 1,$(words $(ORCH_SHARDS))),$(filter $(ORCH_SHARD_COUNTS),$(ORCH_SHARDS))),,$(error ORCH_SHARDS must be a whole number from 1 to 32, got "$(ORCH_SHARDS)"))

# $(call orch_shards,SELECTOR,TIMEOUT,FLAGS,OTHERS) runs the ORCH_PKG tests that SELECTOR
# matches, dealt round-robin into ORCH_SHARDS go test processes that run at once (fewer when
# there are fewer tests than shards, so no shard is empty). FLAGS goes to every go test it
# starts (test-fast passes -short). If OTHERS is a package pattern, every package it names
# except ORCH_PKG runs in one more go test alongside the shards. It fails if any of them fails.
# Each process's output is printed whole once all have finished.
#
# It fails closed, before any test starts: on a bad ORCH_SHARDS, a list that does not build, a
# selector that matches nothing, a `go list` that fails, or shard lists that are not exactly
# the listed tests, each once and none empty. After dispatch it checks that one process started
# per shard list. No step's status passes through a pipe, where sh would drop it.
define orch_shards
	@$(orch_shards_check)set -e; dir=$$(mktemp -d); trap 'rm -rf "$$dir"' EXIT; pids=; others=; \
	go test $(3) -list $(1) $(ORCH_PKG) > "$$dir/list" 2>&1 || { cat "$$dir/list"; exit 1; }; \
	grep -E '^(Test|Fuzz|Example)' "$$dir/list" > "$$dir/names" || true; \
	total=$$(awk 'END { print NR }' "$$dir/names"); \
	if [ "$$total" -eq 0 ]; then echo "orch_shards: the selector matches no test in $(ORCH_PKG)" >&2; exit 1; fi; \
	n=$(ORCH_SHARDS); if [ "$$n" -gt "$$total" ]; then n=$$total; fi; \
	awk -v n="$$n" '{ i = (NR - 1) % n; s[i] = (i in s) ? s[i] "|" $$0 : $$0 } END { for (i = 0; i < n; i++) print s[i] }' "$$dir/names" > "$$dir/shards"; \
	lists=$$(awk 'END { print NR }' "$$dir/shards"); empty=$$(awk '$$0 == "" { c++ } END { print c + 0 }' "$$dir/shards"); \
	tr '|' '\n' < "$$dir/shards" > "$$dir/dealt"; sort "$$dir/dealt" > "$$dir/dealt.sorted"; sort "$$dir/names" > "$$dir/names.sorted"; \
	if [ "$$lists" -ne "$$n" ] || [ "$$empty" -ne 0 ] || ! cmp -s "$$dir/dealt.sorted" "$$dir/names.sorted"; then \
	  echo "orch_shards: the $$total listed tests were not dealt into $$n non-empty shard lists, each test once ($$lists lists, $$empty empty)" >&2; exit 1; \
	fi; \
	if [ -n "$(4)" ]; then \
	  orch=$$(go list $(ORCH_PKG)); all=$$(go list $(4)); \
	  others=$$(printf '%s\n' $$all | grep -vxF "$$orch" || true); \
	fi; \
	echo "$(ORCH_PKG): $$total tests in $$n shards"; \
	if [ -n "$$others" ]; then \
	  go test $(3) $(TESTFLAGS) -timeout $(2) $$others < /dev/null > "$$dir/others" 2>&1 & pids="$$!"; \
	fi; \
	i=0; while IFS= read -r sel; do \
	  go test $(3) $(TESTFLAGS) -timeout $(2) -run "^($$sel)\$$" $(ORCH_PKG) < /dev/null > "$$dir/shard$$i" 2>&1 & pids="$$pids $$!"; \
	  i=$$((i + 1)); \
	done < "$$dir/shards"; \
	rc=0; if [ "$$i" -ne "$$n" ]; then echo "orch_shards: started $$i shard processes, want $$n" >&2; rc=1; fi; \
	for p in $$pids; do wait "$$p" || rc=1; done; \
	for f in "$$dir/others" "$$dir"/shard*; do if [ -f "$$f" ]; then cat "$$f"; fi; done; exit $$rc
endef

# Full test suite — every test, including the slow internal/orchestrator integration
# (e2e) tests that drive real tmux/git/rebase/validate. This is the authoritative gate: CI
# runs it on every push/PR (.github/workflows/ci.yml). TESTFLAGS lets CI add -race without
# changing the default local invocation. internal/orchestrator runs sharded, and every other
# package runs in one go test alongside the shards.
test:
	$(call orch_shards,.,$(TEST_TIMEOUT),,./...)

# The gate's own proofs. `.ttorch/validate.sh` runs this ON TOP of test-fast, because the
# fast lane skips them: 16 of the 18 tests in gateattacks_test.go reach deliveryHarness,
# which calls skipIfShort, so `go test -short` runs none of the end-to-end attacks. ci.yml
# fires on push to main and on pull_request, so a branch with no PR is not covered there
# either, and the attack suite that justifies the whole trust gate ran in NEITHER lane that
# gates a merge.
#
# Deliberately NOT -short. These are the tests that demonstrate the guard refuses the
# attacks it exists to refuse; a gate that does not run them is asserting its own
# correctness. Measured cost below in test-gate's own run time.
#
# This line is GENERATED, not maintained. TestGateTestsSelectorCoversTheProofs derives it
# from the proof files that exist and compares it byte for byte, printing the replacement
# on a mismatch. Do not hand-edit it: add or rename a proof and paste what the test prints.
GATE_TESTS = '^(TestApprovalPayloadScope|TestApprovalPayload_RoundTripsEveryGrantablePath|TestApprove_RefusesUnicodeSpaceInGrantPath|TestDiffFiles_RenameKeepsTheCodeVisible|TestDiffGateConfigHits_AgreesWithFrozenGuard|TestEmbeddedPayloadIsNotAssignable|TestEmbedsContentRoot_GlobsAndQuotes|TestEntriesWithDirs|TestEveryInstalledContentFileIsCovered|TestFSIdentityKeyMatchesTheFilesystem|TestFSIdentityKeySweep|TestGateConfigCoversTheDecidingCode|TestGateConfigFilesAreRealPaths|TestGateCostFiguresMatchTheDoc|TestGateGuard_BlobErasesCoveredDirectory|TestGateGuard_CollisionOutsideTheGateSet|TestGateGuard_ControlCharacterPathRefused|TestGateGuard_EmbeddedContentInstallChannels|TestGateGuard_FullFoldMakefileSubstitution|TestGateGuard_LearningsLedgerIsAWriteChannelIntoAGENTS|TestGateGuard_LinksOverCoveredGround|TestGateGuard_OrdinaryChangeStillMergesAfterRound6|TestGateGuard_OrdinaryRenameStillMerges|TestGateGuard_PreExistingCollisionDoesNotBlock|TestGateGuard_ProjectConfigAndNestedInstructions|TestGateGuard_PublishedInstallersNeedAllowGateChange|TestGateGuard_RenameReportsBothSides|TestGateGuard_SymlinkAtCoveredDirectory|TestGateGuard_SymlinkSwapNeedsAllowGateChange|TestGateGuard_TagNamedLikeTheDefaultBranchShadowsNothing|TestGateGuard_ToolchainRedirectNeedsAllowGateChange|TestGateGuard_UnicodeFoldCollisionAttack|TestGateLaneRunsTheEvidencePackages|TestGateScopeFailsClosed|TestGateScope_ContentOnlyGatesTheRepoThatEmbedsIt|TestGateScope_GitattributesCannotUnScopeTheRepo|TestGateTestsSelectorCoversTheProofs|TestGateTestsShardsRunEveryProofOnce|TestGate_RefusesWithoutARecordedDefaultBranch|TestGrantablePath_RefusesEveryFieldsSeparator|TestInstallerExposesNoFSChoice|TestLand_GateChangeApprovalOffPrintsTheUnapprovedLine|TestLand_GateChangeApprovalUnrecognizedValueIsNamed|TestLand_RebaseOntoAnOriginAheadOfLocalStillLands|TestLand_RefusesARemoteTrackingRefTheFetchDidNotSet|TestLand_WarnsWhenTheDefaultBranchLostTheLastLandedCommit|TestMatchesGateConfig|TestMatchesGateConfig_CoveredDirectoryOwnPath|TestMatchesGateConfig_FullFoldSpellings|TestMergeLocal_AllowGateChangeAuthorizesGateConfigChange|TestMergeLocal_DecidingCodeChangeNeedsAllowGateChange|TestMergeLocal_GateBaseTagCannotHideABlockingHit|TestMergeLocal_GateBaseTagCannotSupplyThePolicy|TestMergeLocal_GateBaseTagCannotSupplyTheValidateScript|TestMergeLocal_GateChangeApprovalOffAcceptsAllowGateChange|TestMergeLocal_GateChangeApprovalOffAutoMergesGateChange|TestMergeLocal_GateChangeApprovalOffNeedsTrustedMode|TestMergeLocal_GateChangeApprovalOffStillRefusesBlockingHit|TestMergeLocal_GateChangeApprovalRequiredOnMainNeedsAllowGateChange|TestMergeLocal_GateChangeApprovalWorkerCannotBindItsOwnMerge|TestMergeLocal_GateChangeApprovalWorkerCannotUnbindItsOwnMerge|TestMergeLocal_GateChangeGrantIsBoundToItsFiles|TestMergeLocal_GateChangeGrantRefusesPathCoveredAfterApproval|TestMergeLocal_GateConfigChangeRefusedWithoutAllowGateChange|TestMergeLocal_GateInstructionChangeNeedsAllowGateChange|TestMergeLocal_VerdictBaseIsTheOnePrepStaged|TestMergeLocal_VerdictFromAMovedDefaultBranchCannotMerge|TestMergeLocal_VerdictWithNoReviewBaseIsRefused|TestNoEmbedRootOutsideContent|TestNotCoveredListIsHonest|TestOnTopicTestsLiveInAProofFile|TestOrchestratorFilesAreClassified|TestReadGateChangePolicy_ReadsTheDefaultBranch|TestSanitizeAuditLine|TestTrailingNFDWouldBeAFalsePositive|TestTreeHasNoUncoveredSymlinks|TestTrustPrep_GateBaseTagCannotEmptyTheReviewDiff|TestTrustPrep_PlantedMainCannotBeTheGateBase|TestTrustRecord_RepointedOriginHeadCannotSupplyThePolicy|TestTrustRecord_StaleMainCannotSupplyThePolicy|TestTtorchRepoIsScopedIn|TestTtorchRuntimeFileIsIgnored|TestTtorchSourceListIsHonest|TestVerdictBaseCovers)$$'

# orch_shards lists what GATE_TESTS matches and runs each of those tests in exactly one of
# ORCH_SHARDS processes; a selector that matches nothing fails rather than passing empty.
# ./internal/worktree/ runs alongside the shards, unfiltered, and the package is derived
# rather than chosen: TestGateLaneRunsTheEvidencePackages reads the guard's own call closure
# for the packages it reads the repository through, and fails if one is missing here or is
# run behind a -run selector that matches none of its tests and so exits 0 having run nothing.
test-gate:
	$(call orch_shards,$(GATE_TESTS),$(TEST_GATE_TIMEOUT),,./internal/worktree/)

# Fast lane — `-short` skips the slow internal/orchestrator integration tests, leaving the
# unit coverage that finishes in seconds. internal/orchestrator's -short tests still take the
# longest of any package, so they run sharded like the full suite's. Used by the local trusted gate
# (.ttorch/validate.sh) for quick turnaround. NOT a replacement for `make test`: the full
# suite (incl. those e2e tests) still runs in CI before anything can land, so the gate is
# not weakened.
test-fast:
	$(call orch_shards,.,$(TEST_FAST_TIMEOUT),-short,./...)

vet:
	go vet ./...

fmt:
	gofmt -w .

fmtcheck:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

lint: vet fmtcheck

dist:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  out=dist/ttorch-$(VERSION)-$$os-$$arch; \
	  echo "building $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o $$out/ttorch ./cmd/ttorch; \
	  tar -C $$out -czf $$out.tar.gz ttorch; \
	  rm -rf $$out; \
	done
	@cd dist && (command -v sha256sum >/dev/null 2>&1 && sha256sum *.tar.gz || shasum -a 256 *.tar.gz) > checksums.txt
	@echo "dist/ ready"

clean:
	rm -rf bin dist
