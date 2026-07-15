package database

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestSQLiteImageStudioTablesAndPersistence(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	for table, columns := range map[string][]string{
		"image_prompt_templates": {"id", "name", "prompt", "model", "size", "quality", "output_format", "background", "style", "tags", "favorite", "usage_count", "last_used_at", "created_at", "updated_at"},
		"image_generation_jobs":  {"id", "status", "prompt", "params_json", "api_key_id", "api_key_name", "api_key_masked", "error_message", "duration_ms", "created_at", "started_at", "completed_at"},
		"image_assets":           {"id", "job_id", "template_id", "filename", "storage_path", "mime_type", "bytes", "width", "height", "model", "requested_size", "actual_size", "quality", "output_format", "revised_prompt", "created_at"},
	} {
		got, err := db.sqliteTableColumns(ctx, table)
		if err != nil {
			t.Fatalf("sqliteTableColumns(%s) 返回错误: %v", table, err)
		}
		for _, column := range columns {
			if _, ok := got[column]; !ok {
				t.Fatalf("%s 缺少列 %q", table, column)
			}
		}
	}

	templateID, err := db.InsertImagePromptTemplate(ctx, ImagePromptTemplateInput{
		Name:         "Sticker",
		Prompt:       "draw a tiny rocket sticker",
		Model:        "gpt-image-2-2k",
		Size:         "auto",
		Quality:      "high",
		OutputFormat: "png",
		Background:   "transparent",
		Style:        "flat",
		Tags:         []string{"贴纸", "贴纸", "Icon"},
		Favorite:     true,
	})
	if err != nil {
		t.Fatalf("InsertImagePromptTemplate 返回错误: %v", err)
	}
	template, err := db.GetImagePromptTemplate(ctx, templateID)
	if err != nil {
		t.Fatalf("GetImagePromptTemplate 返回错误: %v", err)
	}
	if template.Name != "Sticker" || template.Model != "gpt-image-2-2k" || !template.Favorite {
		t.Fatalf("template = %#v", template)
	}
	if len(template.Tags) != 2 || template.Tags[0] != "贴纸" || template.Tags[1] != "Icon" {
		t.Fatalf("template tags = %#v", template.Tags)
	}

	list, err := db.ListImagePromptTemplates(ctx, "rocket", "贴纸")
	if err != nil {
		t.Fatalf("ListImagePromptTemplates 返回错误: %v", err)
	}
	if len(list) != 1 || list[0].ID != templateID {
		t.Fatalf("filtered templates = %#v", list)
	}

	if err := db.IncrementImagePromptTemplateUsage(ctx, templateID); err != nil {
		t.Fatalf("IncrementImagePromptTemplateUsage 返回错误: %v", err)
	}
	template, err = db.GetImagePromptTemplate(ctx, templateID)
	if err != nil {
		t.Fatalf("GetImagePromptTemplate after usage 返回错误: %v", err)
	}
	if template.UsageCount != 1 || template.LastUsedAt.IsZero() {
		t.Fatalf("usage fields = count %d last %v", template.UsageCount, template.LastUsedAt)
	}

	if err := db.UpdateImagePromptTemplate(ctx, templateID, ImagePromptTemplateInput{
		Name:         "Updated",
		Prompt:       "updated prompt",
		Model:        "gpt-image-2-4k",
		Size:         "3840x2160",
		Quality:      "medium",
		OutputFormat: "webp",
		Tags:         []string{"壁纸"},
	}); err != nil {
		t.Fatalf("UpdateImagePromptTemplate 返回错误: %v", err)
	}
	template, err = db.GetImagePromptTemplate(ctx, templateID)
	if err != nil {
		t.Fatalf("GetImagePromptTemplate after update 返回错误: %v", err)
	}
	if template.Name != "Updated" || template.Model != "gpt-image-2-4k" || template.OutputFormat != "webp" {
		t.Fatalf("updated template = %#v", template)
	}

	jobID, err := db.InsertImageGenerationJob(ctx, ImageGenerationJobInput{
		Prompt:       "updated prompt",
		ParamsJSON:   `{"model":"gpt-image-2-4k"}`,
		APIKeyID:     7,
		APIKeyName:   "Team A",
		APIKeyMasked: "sk-****",
	})
	if err != nil {
		t.Fatalf("InsertImageGenerationJob 返回错误: %v", err)
	}
	job, err := db.GetImageGenerationJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetImageGenerationJob 返回错误: %v", err)
	}
	if job.Status != ImageJobQueued || job.APIKeyID != 7 || job.ParamsJSON == "" {
		t.Fatalf("queued job = %#v", job)
	}
	if err := db.UpdateImageGenerationJobParamsJSON(ctx, jobID, `{"output_format":"jpeg"}`); err != nil {
		t.Fatalf("UpdateImageGenerationJobParamsJSON 返回错误: %v", err)
	}
	job, err = db.GetImageGenerationJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetImageGenerationJob after params update 返回错误: %v", err)
	}
	if job.ParamsJSON != `{"output_format":"jpeg"}` {
		t.Fatalf("params_json = %q, want jpeg fallback params", job.ParamsJSON)
	}
	if err := db.MarkImageJobRunning(ctx, jobID); err != nil {
		t.Fatalf("MarkImageJobRunning 返回错误: %v", err)
	}
	if err := db.MarkImageJobSucceeded(ctx, jobID, 1234); err != nil {
		t.Fatalf("MarkImageJobSucceeded 返回错误: %v", err)
	}

	assetID, err := db.InsertImageAsset(ctx, ImageAssetInput{
		JobID:         jobID,
		TemplateID:    templateID,
		Filename:      "1-01-test.png",
		StoragePath:   filepath.Join(t.TempDir(), "1-01-test.png"),
		MimeType:      "image/png",
		Bytes:         2048,
		Width:         1024,
		Height:        1024,
		Model:         "gpt-image-2-4k",
		RequestedSize: "3840x2160",
		ActualSize:    "1024x1024",
		Quality:       "high",
		OutputFormat:  "png",
		RevisedPrompt: "revised prompt",
	})
	if err != nil {
		t.Fatalf("InsertImageAsset 返回错误: %v", err)
	}
	job, err = db.GetImageGenerationJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetImageGenerationJob with assets 返回错误: %v", err)
	}
	if job.Status != ImageJobSucceeded || job.DurationMs != 1234 || job.CompletedAt == nil || len(job.Assets) != 1 {
		t.Fatalf("succeeded job = %#v", job)
	}
	if job.Assets[0].ID != assetID || job.Assets[0].TemplateID != templateID || job.Assets[0].Width != 1024 {
		t.Fatalf("job asset = %#v", job.Assets[0])
	}

	page, err := db.ListImageAssets(ctx, 1, 10, 0)
	if err != nil {
		t.Fatalf("ListImageAssets 返回错误: %v", err)
	}
	if page.Total != 1 || len(page.Assets) != 1 {
		t.Fatalf("asset page = %#v", page)
	}
	jobs, err := db.ListImageGenerationJobs(ctx, 1, 10, 0)
	if err != nil {
		t.Fatalf("ListImageGenerationJobs 返回错误: %v", err)
	}
	if jobs.Total != 1 || len(jobs.Jobs) != 1 || len(jobs.Jobs[0].Assets) != 1 {
		t.Fatalf("job page = %#v", jobs)
	}

	// Portal isolation: filter by api_key_id
	otherJobID, err := db.InsertImageGenerationJob(ctx, ImageGenerationJobInput{
		Prompt:       "other key job",
		ParamsJSON:   `{"model":"gpt-image-2"}`,
		APIKeyID:     99,
		APIKeyName:   "Other",
		APIKeyMasked: "sk-other",
	})
	if err != nil {
		t.Fatalf("InsertImageGenerationJob other 返回错误: %v", err)
	}
	filteredJobs, err := db.ListImageGenerationJobs(ctx, 1, 10, 7)
	if err != nil {
		t.Fatalf("ListImageGenerationJobs by api_key 返回错误: %v", err)
	}
	if filteredJobs.Total != 1 || len(filteredJobs.Jobs) != 1 || filteredJobs.Jobs[0].APIKeyID != 7 {
		t.Fatalf("filtered jobs = %#v", filteredJobs)
	}
	filteredAssets, err := db.ListImageAssets(ctx, 1, 10, 7)
	if err != nil {
		t.Fatalf("ListImageAssets by api_key 返回错误: %v", err)
	}
	if filteredAssets.Total != 1 || len(filteredAssets.Assets) != 1 {
		t.Fatalf("filtered assets = %#v", filteredAssets)
	}
	emptyAssets, err := db.ListImageAssets(ctx, 1, 10, 99)
	if err != nil {
		t.Fatalf("ListImageAssets empty key 返回错误: %v", err)
	}
	if emptyAssets.Total != 0 {
		t.Fatalf("empty key assets total = %d, want 0", emptyAssets.Total)
	}
	ownerKeyID, err := db.GetImageAssetJobAPIKeyID(ctx, assetID)
	if err != nil {
		t.Fatalf("GetImageAssetJobAPIKeyID 返回错误: %v", err)
	}
	if ownerKeyID != 7 {
		t.Fatalf("ownerKeyID = %d, want 7", ownerKeyID)
	}
	_ = otherJobID

	if err := db.DeleteImageAsset(ctx, assetID); err != nil {
		t.Fatalf("DeleteImageAsset 返回错误: %v", err)
	}
	if _, err := db.GetImageAsset(ctx, assetID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetImageAsset after delete err = %v, want sql.ErrNoRows", err)
	}
	jobID2, err := db.InsertImageGenerationJob(ctx, ImageGenerationJobInput{
		Prompt:     "delete this job",
		ParamsJSON: `{"model":"gpt-image-2"}`,
	})
	if err != nil {
		t.Fatalf("InsertImageGenerationJob job2 返回错误: %v", err)
	}
	if err := db.MarkImageJobFailed(ctx, jobID2, "upstream failed", 456); err != nil {
		t.Fatalf("MarkImageJobFailed 返回错误: %v", err)
	}
	assetID2, err := db.InsertImageAsset(ctx, ImageAssetInput{
		JobID:       jobID2,
		Filename:    "delete-me.png",
		StoragePath: filepath.Join(t.TempDir(), "delete-me.png"),
		MimeType:    "image/png",
		Bytes:       1024,
	})
	if err != nil {
		t.Fatalf("InsertImageAsset job2 返回错误: %v", err)
	}
	if err := db.DeleteImageGenerationJob(ctx, jobID2); err != nil {
		t.Fatalf("DeleteImageGenerationJob 返回错误: %v", err)
	}
	if _, err := db.GetImageGenerationJob(ctx, jobID2); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetImageGenerationJob after delete err = %v, want sql.ErrNoRows", err)
	}
	if _, err := db.GetImageAsset(ctx, assetID2); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetImageAsset after job delete err = %v, want sql.ErrNoRows", err)
	}
	if err := db.DeleteImagePromptTemplate(ctx, templateID); err != nil {
		t.Fatalf("DeleteImagePromptTemplate 返回错误: %v", err)
	}
}

func TestImageJobsInterruptedOnStartup(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	queuedID, err := db.InsertImageGenerationJob(ctx, ImageGenerationJobInput{Prompt: "queued"})
	if err != nil {
		t.Fatalf("InsertImageGenerationJob queued 返回错误: %v", err)
	}
	runningID, err := db.InsertImageGenerationJob(ctx, ImageGenerationJobInput{Prompt: "running"})
	if err != nil {
		t.Fatalf("InsertImageGenerationJob running 返回错误: %v", err)
	}
	if err := db.MarkImageJobRunning(ctx, runningID); err != nil {
		t.Fatalf("MarkImageJobRunning 返回错误: %v", err)
	}
	if err := db.MarkInterruptedImageJobs(ctx); err != nil {
		t.Fatalf("MarkInterruptedImageJobs 返回错误: %v", err)
	}
	for _, id := range []int64{queuedID, runningID} {
		job, err := db.GetImageGenerationJob(ctx, id)
		if err != nil {
			t.Fatalf("GetImageGenerationJob(%d) 返回错误: %v", id, err)
		}
		if job.Status != ImageJobFailed || job.ErrorMessage == "" || job.CompletedAt == nil {
			t.Fatalf("interrupted job = %#v", job)
		}
	}
}
