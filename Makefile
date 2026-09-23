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

# Full test suite — every test, including the slow internal/orchestrator integration
# (e2e) tests that drive real tmux/git/rebase/validate (~100s). This is the authoritative
# gate: CI runs it on every push/PR (.github/workflows/ci.yml). TESTFLAGS lets CI add
# -race without changing the default local invocation.
test:
	go test $(TESTFLAGS) ./...

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
GATE_TESTS = '^(TestDiffFiles_RenameKeepsTheCodeVisible|TestDiffGateConfigHits_AgreesWithFrozenGuard|TestEmbeddedPayloadIsNotAssignable|TestEmbedsContentRoot_GlobsAndQuotes|TestEntriesWithDirs|TestEveryInstalledContentFileIsCovered|TestFSIdentityKeyMatchesTheFilesystem|TestFSIdentityKeySweep|TestGateConfigCoversTheDecidingCode|TestGateConfigFilesAreRealPaths|TestGateCostFiguresMatchTheDoc|TestGateGuard_BlobErasesCoveredDirectory|TestGateGuard_CollisionOutsideTheGateSet|TestGateGuard_ControlCharacterPathRefused|TestGateGuard_EmbeddedContentInstallChannels|TestGateGuard_FullFoldMakefileSubstitution|TestGateGuard_LearningsLedgerIsAWriteChannelIntoAGENTS|TestGateGuard_LinksOverCoveredGround|TestGateGuard_OrdinaryChangeStillMergesAfterRound6|TestGateGuard_OrdinaryRenameStillMerges|TestGateGuard_PreExistingCollisionDoesNotBlock|TestGateGuard_ProjectConfigAndNestedInstructions|TestGateGuard_PublishedInstallersNeedAllowGateChange|TestGateGuard_RenameReportsBothSides|TestGateGuard_SymlinkAtCoveredDirectory|TestGateGuard_SymlinkSwapNeedsAllowGateChange|TestGateGuard_ToolchainRedirectNeedsAllowGateChange|TestGateGuard_UnicodeFoldCollisionAttack|TestGateLaneRunsTheEvidencePackages|TestGateScopeFailsClosed|TestGateScope_ContentOnlyGatesTheRepoThatEmbedsIt|TestGateScope_GitattributesCannotUnScopeTheRepo|TestGateTestsSelectorCoversTheProofs|TestInstallerExposesNoFSChoice|TestMatchesGateConfig|TestMatchesGateConfig_CoveredDirectoryOwnPath|TestMatchesGateConfig_FullFoldSpellings|TestMergeLocal_AllowGateChangeAuthorizesGateConfigChange|TestMergeLocal_DecidingCodeChangeNeedsAllowGateChange|TestMergeLocal_GateChangeGrantIsBoundToItsFiles|TestMergeLocal_GateChangeGrantRefusesPathCoveredAfterApproval|TestMergeLocal_GateConfigChangeRefusedWithoutAllowGateChange|TestMergeLocal_GateInstructionChangeNeedsAllowGateChange|TestNoEmbedRootOutsideContent|TestNotCoveredListIsHonest|TestOnTopicTestsLiveInAProofFile|TestOrchestratorFilesAreClassified|TestSanitizeAuditLine|TestTrailingNFDWouldBeAFalsePositive|TestTreeHasNoUncoveredSymlinks|TestTtorchRepoIsScopedIn|TestTtorchRuntimeFileIsIgnored|TestTtorchSourceListIsHonest)$$'

# The second line is unfiltered, and the package is derived rather than chosen:
# TestGateLaneRunsTheEvidencePackages reads the guard's own call closure for the packages it
# reads the repository through, and fails if one is missing here or is run behind a -run
# selector that matches none of its tests and so exits 0 having run nothing.
test-gate:
	go test $(TESTFLAGS) -run $(GATE_TESTS) ./internal/orchestrator/
	go test $(TESTFLAGS) ./internal/worktree/

# Fast lane — `-short` skips the slow internal/orchestrator integration tests, leaving the
# unit coverage that finishes in seconds. Used by the local trusted gate
# (.ttorch/validate.sh) for quick turnaround. NOT a replacement for `make test`: the full
# suite (incl. those e2e tests) still runs in CI before anything can land, so the gate is
# not weakened.
test-fast:
	go test -short $(TESTFLAGS) ./...

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
