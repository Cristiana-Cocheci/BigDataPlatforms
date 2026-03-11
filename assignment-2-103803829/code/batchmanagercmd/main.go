package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultTenantConfigDir        = "./tenant_configs"
	supportedBatchmanagerTenant   = "tenant2"
	tenant2SilverService          = "tenant2-silverpipeline"
	defaultBatchmanagerInputGlob  = "*_bronze_extract.csv"
	defaultBatchmanagerSilverGlob = "*_silver_hourly.csv"
	defaultBatchmanagerStateFile  = ".batchmanager_state.json"
	silverPipelineModeEnv         = "SILVER_PIPELINE_MODE"
	silverPipelineInputFilesEnv   = "SILVER_PIPELINE_INPUT_FILES"
	silverPipelineDayEnv          = "SILVER_PIPELINE_DAY"
	silverPipelineModeExtract     = "extract-cache"
	silverPipelineModeTransform   = "transform-cache"
	silverPipelineStorageLocal    = "local"
)

type silverPipelineConfig struct {
	TenantID string                `yaml:"tenant_id"`
	Pipeline silverPipelineRuntime `yaml:"pipeline"`
}

type silverPipelineRuntime struct {
	StorageBackend string                   `yaml:"storage_backend"`
	CacheDir       string                   `yaml:"cache_dir"`
	BatchManager   silverPipelineBatchModel `yaml:"batchmanager"`
}

type silverPipelineBatchModel struct {
	InputGlob string `yaml:"input_glob"`
	StateFile string `yaml:"state_file"`
}

type batchManagerState struct {
	Files map[string]batchManagerFileState `json:"files"`
}

type batchManagerFileState struct {
	Size        int64  `json:"size"`
	ModTimeUnix int64  `json:"mod_time_unix"`
	ProcessedAt string `json:"processed_at"`
}

type cacheFileRecord struct {
	Name        string
	Path        string
	Size        int64
	ModTimeUnix int64
}

func main() {
	command := flag.String("command", "run", "Command: status | run | extract-cache | cleanup-processed")
	tenantID := flag.String("tenant", supportedBatchmanagerTenant, "Tenant ID supported by this batchmanager")
	force := flag.Bool("force", false, "Reprocess all matching cache files regardless of batchmanager state")
	build := flag.Bool("build", false, "Build the tenant silverpipeline image before invoking it")
	extractDay := flag.String("day", "", "Required for extract-cache: day partition to extract from bronze data (YYYY-MM-DD)")
	composeFilesRaw := flag.String("compose-files", "docker-compose.yml,docker-compose.multitenant-brokers.yml", "Comma-separated docker compose files")

	flag.Parse()

	if strings.ToLower(strings.TrimSpace(*tenantID)) != supportedBatchmanagerTenant {
		fatalf("mysimbdp-batchmanager currently supports only tenant=%s", supportedBatchmanagerTenant)
	}

	composeFiles := parseList(*composeFilesRaw)
	if len(composeFiles) == 0 {
		fatalf("compose-files cannot be empty")
	}

	pipelineConfig, err := loadSilverPipelineConfig(*tenantID)
	if err != nil {
		fatalf("failed to load silver pipeline config: %v", err)
	}

	hostCacheDir, err := filepath.Abs(pipelineConfig.Pipeline.CacheDir)
	if err != nil {
		fatalf("failed to resolve cache dir %s: %v", pipelineConfig.Pipeline.CacheDir, err)
	}

	stateFilePath := filepath.Join(hostCacheDir, pipelineConfig.Pipeline.BatchManager.StateFile)

	switch *command {
	case "status":
		if err := showBatchStatus(hostCacheDir, pipelineConfig.Pipeline.BatchManager.InputGlob, stateFilePath); err != nil {
			fatalf("status failed: %v", err)
		}
	case "extract-cache":
		resolvedExtractDay, err := parseRequiredExtractDay(*extractDay)
		if err != nil {
			fatalf("extract-cache failed: %v", err)
		}

		if err := invokeSilverPipeline(composeFiles, *build, silverPipelineModeExtract, nil, resolvedExtractDay); err != nil {
			fatalf("extract-cache failed: %v", err)
		}
		fmt.Printf("tenant=%s cache refresh completed day=%s\n", supportedBatchmanagerTenant, resolvedExtractDay)
	case "run":
		if err := runBatch(hostCacheDir, pipelineConfig.Pipeline.BatchManager.InputGlob, stateFilePath, composeFiles, *build, *force); err != nil {
			fatalf("run failed: %v", err)
		}
	case "cleanup-processed":
		if err := cleanupProcessedCacheFiles(hostCacheDir, pipelineConfig.Pipeline.BatchManager.InputGlob, stateFilePath); err != nil {
			fatalf("cleanup-processed failed: %v", err)
		}
	default:
		fatalf("invalid command: %s (expected: status | run | extract-cache | cleanup-processed)", *command)
	}
}

func runBatch(cacheDir string, inputGlob string, stateFilePath string, composeFiles []string, build bool, force bool) error {
	matchedFiles, err := discoverCacheFiles(cacheDir, inputGlob)
	if err != nil {
		return err
	}

	state, err := loadBatchManagerState(stateFilePath)
	if err != nil {
		return err
	}

	pendingFiles := pendingCacheFiles(matchedFiles, state, force)
	if len(pendingFiles) == 0 {
		managedFiles, err := discoverManagedCacheFiles(cacheDir, inputGlob)
		if err != nil {
			return err
		}

		recordedAt := time.Now().UTC().Format(time.RFC3339)
		if recordFilesInState(&state, managedFiles, recordedAt) > 0 {
			if err := saveBatchManagerState(stateFilePath, state); err != nil {
				return err
			}
		}

		fmt.Printf("tenant=%s no pending cache files in %s\n", supportedBatchmanagerTenant, cacheDir)
		return nil
	}

	inputNames := make([]string, 0, len(pendingFiles))
	for _, file := range pendingFiles {
		inputNames = append(inputNames, file.Name)
	}

	if err := invokeSilverPipeline(composeFiles, build, silverPipelineModeTransform, inputNames, ""); err != nil {
		return err
	}

	managedFiles, err := discoverManagedCacheFiles(cacheDir, inputGlob)
	if err != nil {
		return err
	}

	processedAt := time.Now().UTC().Format(time.RFC3339)
	recordFilesInState(&state, managedFiles, processedAt)

	if err := saveBatchManagerState(stateFilePath, state); err != nil {
		return err
	}

	fmt.Printf("tenant=%s processed_files=%d state_file=%s\n", supportedBatchmanagerTenant, len(pendingFiles), stateFilePath)
	return nil
}

func showBatchStatus(cacheDir string, inputGlob string, stateFilePath string) error {
	matchedFiles, err := discoverCacheFiles(cacheDir, inputGlob)
	if err != nil {
		return err
	}

	state, err := loadBatchManagerState(stateFilePath)
	if err != nil {
		return err
	}

	pendingFiles := pendingCacheFiles(matchedFiles, state, false)
	pendingSet := make(map[string]struct{}, len(pendingFiles))
	for _, file := range pendingFiles {
		pendingSet[file.Name] = struct{}{}
	}

	fmt.Printf("tenant=%s cache_dir=%s matched=%d pending=%d state_file=%s\n", supportedBatchmanagerTenant, cacheDir, len(matchedFiles), len(pendingFiles), stateFilePath)
	for _, file := range matchedFiles {
		status := "processed"
		if _, isPending := pendingSet[file.Name]; isPending {
			status = "pending"
		}

		processedAt := ""
		if stateEntry, exists := state.Files[file.Name]; exists {
			processedAt = stateEntry.ProcessedAt
		}

		fmt.Printf("- %s file=%s size=%d processed_at=%s\n", status, file.Name, file.Size, processedAt)
	}

	return nil
}

func cleanupProcessedCacheFiles(cacheDir string, inputGlob string, stateFilePath string) error {
	matchedFiles, err := discoverManagedCacheFiles(cacheDir, inputGlob)
	if err != nil {
		return err
	}

	state, err := loadBatchManagerState(stateFilePath)
	if err != nil {
		return err
	}

	deletedFiles := 0
	recordedAt := time.Now().UTC().Format(time.RFC3339)
	if state.Files == nil {
		state.Files = make(map[string]batchManagerFileState)
	}

	for _, file := range matchedFiles {
		if err := os.Remove(file.Path); err != nil {
			return fmt.Errorf("failed to delete cache file %s: %w", file.Path, err)
		}

		state.Files[file.Name] = batchManagerFileState{
			Size:        file.Size,
			ModTimeUnix: file.ModTimeUnix,
			ProcessedAt: recordedAt,
		}
		deletedFiles++

		fmt.Printf("- deleted cache file=%s size=%d recorded_at=%s\n", file.Name, file.Size, recordedAt)
	}

	if len(matchedFiles) > 0 {
		if err := saveBatchManagerState(stateFilePath, state); err != nil {
			return err
		}
	}

	fmt.Printf("tenant=%s cleanup-processed completed cache_dir=%s matched=%d deleted=%d state_file=%s\n", supportedBatchmanagerTenant, cacheDir, len(matchedFiles), deletedFiles, stateFilePath)
	return nil
}

func discoverManagedCacheFiles(cacheDir string, inputGlob string) ([]cacheFileRecord, error) {
	globs := []string{inputGlob, defaultBatchmanagerSilverGlob}
	filesByPath := make(map[string]cacheFileRecord)

	for _, glob := range globs {
		files, err := discoverCacheFiles(cacheDir, glob)
		if err != nil {
			return nil, err
		}

		for _, file := range files {
			filesByPath[file.Path] = file
		}
	}

	mergedFiles := make([]cacheFileRecord, 0, len(filesByPath))
	for _, file := range filesByPath {
		mergedFiles = append(mergedFiles, file)
	}

	sort.Slice(mergedFiles, func(i int, j int) bool {
		return mergedFiles[i].Name < mergedFiles[j].Name
	})

	return mergedFiles, nil
}

func loadSilverPipelineConfig(tenantID string) (silverPipelineConfig, error) {
	configDir := getEnv("TENANT_CONFIG_DIR", defaultTenantConfigDir)
	configPath := filepath.Join(configDir, fmt.Sprintf("silverpipeline_%s.yaml", strings.ToLower(strings.TrimSpace(tenantID))))

	content, err := os.ReadFile(configPath)
	if err != nil {
		return silverPipelineConfig{}, fmt.Errorf("failed to read silver pipeline config %s: %w", configPath, err)
	}

	var config silverPipelineConfig
	if err := yaml.Unmarshal(content, &config); err != nil {
		return silverPipelineConfig{}, fmt.Errorf("failed to parse silver pipeline config %s: %w", configPath, err)
	}

	if strings.TrimSpace(config.TenantID) == "" {
		config.TenantID = supportedBatchmanagerTenant
	}

	if strings.TrimSpace(config.Pipeline.CacheDir) == "" {
		config.Pipeline.CacheDir = filepath.Join("./tenant_caching_dir", supportedBatchmanagerTenant)
	}

	if strings.TrimSpace(config.Pipeline.BatchManager.InputGlob) == "" {
		config.Pipeline.BatchManager.InputGlob = defaultBatchmanagerInputGlob
	}

	if strings.TrimSpace(config.Pipeline.BatchManager.StateFile) == "" {
		config.Pipeline.BatchManager.StateFile = defaultBatchmanagerStateFile
	}

	if strings.ToLower(strings.TrimSpace(config.TenantID)) != supportedBatchmanagerTenant {
		return silverPipelineConfig{}, fmt.Errorf("batchmanager currently supports only tenant=%s", supportedBatchmanagerTenant)
	}

	return config, nil
}

func discoverCacheFiles(cacheDir string, inputGlob string) ([]cacheFileRecord, error) {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create cache dir %s: %w", cacheDir, err)
	}

	pattern := strings.TrimSpace(inputGlob)
	if pattern == "" {
		pattern = defaultBatchmanagerInputGlob
	}

	matches, err := filepath.Glob(filepath.Join(cacheDir, pattern))
	if err != nil {
		return nil, fmt.Errorf("failed to evaluate cache file glob %s: %w", pattern, err)
	}

	sort.Strings(matches)
	files := make([]cacheFileRecord, 0, len(matches))
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil {
			return nil, fmt.Errorf("failed to stat cache file %s: %w", match, err)
		}
		if info.IsDir() {
			continue
		}

		files = append(files, cacheFileRecord{
			Name:        filepath.Base(match),
			Path:        match,
			Size:        info.Size(),
			ModTimeUnix: info.ModTime().Unix(),
		})
	}

	return files, nil
}

func loadBatchManagerState(stateFilePath string) (batchManagerState, error) {
	content, err := os.ReadFile(stateFilePath)
	if errors.Is(err, os.ErrNotExist) {
		return batchManagerState{Files: make(map[string]batchManagerFileState)}, nil
	}
	if err != nil {
		return batchManagerState{}, fmt.Errorf("failed to read batchmanager state %s: %w", stateFilePath, err)
	}

	var state batchManagerState
	if err := json.Unmarshal(content, &state); err != nil {
		return batchManagerState{}, fmt.Errorf("failed to parse batchmanager state %s: %w", stateFilePath, err)
	}

	if state.Files == nil {
		state.Files = make(map[string]batchManagerFileState)
	}

	return state, nil
}

func saveBatchManagerState(stateFilePath string, state batchManagerState) error {
	if err := os.MkdirAll(filepath.Dir(stateFilePath), 0o755); err != nil {
		return fmt.Errorf("failed to create directory for state file %s: %w", stateFilePath, err)
	}

	content, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal batchmanager state: %w", err)
	}

	if err := os.WriteFile(stateFilePath, content, 0o644); err != nil {
		return fmt.Errorf("failed to write batchmanager state %s: %w", stateFilePath, err)
	}

	return nil
}

func pendingCacheFiles(files []cacheFileRecord, state batchManagerState, force bool) []cacheFileRecord {
	pending := make([]cacheFileRecord, 0)
	for _, file := range files {
		if force {
			pending = append(pending, file)
			continue
		}

		stateEntry, exists := state.Files[file.Name]
		if !exists || stateEntry.Size != file.Size || stateEntry.ModTimeUnix != file.ModTimeUnix {
			pending = append(pending, file)
		}
	}

	return pending
}

func recordFilesInState(state *batchManagerState, files []cacheFileRecord, processedAt string) int {
	if state.Files == nil {
		state.Files = make(map[string]batchManagerFileState)
	}

	recorded := 0
	for _, file := range files {
		existing, exists := state.Files[file.Name]
		if exists && existing.Size == file.Size && existing.ModTimeUnix == file.ModTimeUnix && existing.ProcessedAt == processedAt {
			continue
		}
		state.Files[file.Name] = batchManagerFileState{
			Size:        file.Size,
			ModTimeUnix: file.ModTimeUnix,
			ProcessedAt: processedAt,
		}
		recorded++
	}

	return recorded
}

func invokeSilverPipeline(composeFiles []string, build bool, mode string, inputFiles []string, extractDay string) error {
	if err := composeUp(composeFiles, "cassandra1", "cassandra2", "cassandra3"); err != nil {
		return err
	}

	args := composeBaseArgs(composeFiles)
	args = append(args, "--profile", "silver")
	args = append(args, "run", "--rm")
	if build {
		args = append(args, "--build")
	}

	args = append(args, "-e", fmt.Sprintf("%s=%s", silverPipelineModeEnv, mode))
	if len(inputFiles) > 0 {
		args = append(args, "-e", fmt.Sprintf("%s=%s", silverPipelineInputFilesEnv, strings.Join(inputFiles, ",")))
	}

	if strings.TrimSpace(extractDay) != "" {
		args = append(args, "-e", fmt.Sprintf("%s=%s", silverPipelineDayEnv, strings.TrimSpace(extractDay)))
	}

	args = append(args, tenant2SilverService)
	return run("docker", args...)
}

func composeUp(composeFiles []string, services ...string) error {
	args := composeBaseArgs(composeFiles)
	args = append(args, "up", "-d")
	args = append(args, services...)
	return run("docker", args...)
}

func composeBaseArgs(composeFiles []string) []string {
	args := []string{"compose"}
	for _, file := range composeFiles {
		args = append(args, "-f", file)
	}
	return args
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func getEnv(key string, defaultValue string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return defaultValue
}

func parseList(input string) []string {
	parts := strings.Split(input, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func parseRequiredExtractDay(value string) (string, error) {
	normalized := strings.TrimSpace(value)
	if normalized == "" {
		return "", fmt.Errorf("missing --day: extract-cache requires --day YYYY-MM-DD")
	}

	if _, err := time.Parse("2006-01-02", normalized); err != nil {
		return "", fmt.Errorf("invalid --day %q: expected YYYY-MM-DD", normalized)
	}

	return normalized, nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
