package componentreadiness

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	bigquery2 "cloud.google.com/go/bigquery"
	fet "github.com/glycerine/golang-fisher-exact"
	"github.com/sirupsen/logrus"

	"github.com/openshift/sippy/pkg/api"
	crtype "github.com/openshift/sippy/pkg/apis/api/componentreport"
	"github.com/openshift/sippy/pkg/bigquery"
	"github.com/openshift/sippy/pkg/regressionallowances"
	"github.com/openshift/sippy/pkg/util/param"
)

func GetTestDetails(ctx context.Context, client *bigquery.Client, prowURL, gcsBucket string, reqOptions crtype.RequestOptions,
) (crtype.ReportTestDetails, []error) {
	generator := componentReportGenerator{
		client:                           client,
		prowURL:                          prowURL,
		gcsBucket:                        gcsBucket,
		cacheOption:                      reqOptions.CacheOption,
		BaseRelease:                      reqOptions.BaseRelease,
		BaseOverrideRelease:              reqOptions.BaseOverrideRelease,
		SampleRelease:                    reqOptions.SampleRelease,
		RequestTestIdentificationOptions: reqOptions.TestIDOption,
		RequestVariantOptions:            reqOptions.VariantOption,
		RequestAdvancedOptions:           reqOptions.AdvancedOption,
	}

	return api.GetDataFromCacheOrGenerate[crtype.ReportTestDetails](
		ctx,
		generator.client.Cache,
		generator.cacheOption,
		generator.GetComponentReportCacheKey(ctx, "TestDetailsReport~"),
		generator.GenerateTestDetailsReport,
		crtype.ReportTestDetails{})
}

func (c *componentReportGenerator) GenerateTestDetailsReport(ctx context.Context) (crtype.ReportTestDetails, []error) {
	if c.TestID == "" {
		return crtype.ReportTestDetails{}, []error{fmt.Errorf("test_id has to be defined for test details")}
	}
	for _, v := range c.DBGroupBy.List() {
		if _, ok := c.RequestedVariants[v]; !ok {
			return crtype.ReportTestDetails{}, []error{fmt.Errorf("all dbGroupBy variants have to be defined for test details: %s is missing", v)}
		}
	}

	componentJobRunTestReportStatus, errs := c.GenerateJobRunTestReportStatus(ctx)
	if len(errs) > 0 {
		return crtype.ReportTestDetails{}, errs
	}
	var err error
	bqs := NewBigQueryRegressionStore(c.client)
	allRegressions, err := bqs.ListCurrentRegressions(ctx)
	if err != nil {
		errs = append(errs, err)
		return crtype.ReportTestDetails{}, errs
	}

	var baseOverrideReport *crtype.ReportTestDetails
	if c.BaseOverrideRelease.Release != "" && c.BaseOverrideRelease.Release != c.BaseRelease.Release {
		// because internalGenerateTestDetailsReport modifies SampleStatus we need to copy it here
		overrideSampleStatus := map[string][]crtype.JobRunTestStatusRow{}
		for k, v := range componentJobRunTestReportStatus.SampleStatus {
			overrideSampleStatus[k] = v
		}

		overrideReport := c.internalGenerateTestDetailsReport(ctx, componentJobRunTestReportStatus.BaseOverrideStatus, c.BaseOverrideRelease.Release, &c.BaseOverrideRelease.Start, &c.BaseOverrideRelease.End, overrideSampleStatus)
		// swap out the base dates for the override
		overrideReport.GeneratedAt = componentJobRunTestReportStatus.GeneratedAt
		baseOverrideReport = &overrideReport
	}

	c.openRegressions = FilterRegressionsForRelease(allRegressions, c.SampleRelease.Release)
	report := c.internalGenerateTestDetailsReport(ctx, componentJobRunTestReportStatus.BaseStatus, c.BaseRelease.Release, &c.BaseRelease.Start, &c.BaseRelease.End, componentJobRunTestReportStatus.SampleStatus)
	report.GeneratedAt = componentJobRunTestReportStatus.GeneratedAt

	if baseOverrideReport != nil {
		baseOverrideReport.BaseOverrideReport = crtype.ReportTestOverride{
			ReportTestStats: report.ReportTestStats,
			JobStats:        report.JobStats,
		}

		return *baseOverrideReport, nil
	}

	return report, nil
}

func (c *componentReportGenerator) GenerateJobRunTestReportStatus(ctx context.Context) (crtype.JobRunTestReportStatus, []error) {
	before := time.Now()
	componentJobRunTestReportStatus, errs := c.getJobRunTestStatusFromBigQuery(ctx)
	if len(errs) > 0 {
		return crtype.JobRunTestReportStatus{}, errs
	}
	logrus.Infof("getJobRunTestStatusFromBigQuery completed in %s with %d sample results and %d base results from db", time.Since(before), len(componentJobRunTestReportStatus.SampleStatus), len(componentJobRunTestReportStatus.BaseStatus))
	now := time.Now()
	componentJobRunTestReportStatus.GeneratedAt = &now
	return componentJobRunTestReportStatus, nil
}

// filterByCrossCompareVariants adds the where clause for any variants being cross-compared (which are not included in RequestedVariants).
// As a side effect, it also appends any necessary parameters for the clause.
func filterByCrossCompareVariants(crossCompare []string, variantGroups map[string][]string, params *[]bigquery2.QueryParameter) (whereClause string) {
	if len(variantGroups) == 0 {
		return // avoid possible nil pointer dereference
	}
	sort.StringSlice(crossCompare).Sort()
	for _, group := range crossCompare {
		if variants := variantGroups[group]; len(variants) > 0 {
			group = param.Cleanse(group)
			paramName := "CrossVariants" + group
			whereClause += fmt.Sprintf(` AND jv_%s.variant_value IN UNNEST(@%s)`, group, paramName)
			*params = append(*params, bigquery2.QueryParameter{
				Name:  paramName,
				Value: variants,
			})
		}
	}
	return
}

func (c *componentReportGenerator) getBaseJobRunTestStatus(
	ctx context.Context,
	allJobVariants crtype.JobVariants,
	baseRelease string,
	baseStart time.Time,
	baseEnd time.Time) (map[string][]crtype.JobRunTestStatusRow, []error) {

	generator := newBaseTestDetailsQueryGenerator(
		c,
		allJobVariants,
		baseRelease,
		baseStart,
		baseEnd,
	)

	jobRunTestStatus, errs := api.GetDataFromCacheOrGenerate[crtype.JobRunTestReportStatus](
		ctx,
		generator.ComponentReportGenerator.client.Cache, generator.cacheOption,
		api.GetPrefixedCacheKey("BaseJobRunTestStatus~", generator),
		generator.queryTestStatus,
		crtype.JobRunTestReportStatus{})

	if len(errs) > 0 {
		return nil, errs
	}

	return jobRunTestStatus.BaseStatus, nil
}

func (c *componentReportGenerator) getSampleJobRunTestStatus(ctx context.Context, allJobVariants crtype.JobVariants) (map[string][]crtype.JobRunTestStatusRow, []error) {

	generator := newSampleTestDetailsQueryGenerator(c, allJobVariants)

	jobRunTestStatus, errs := api.GetDataFromCacheOrGenerate[crtype.JobRunTestReportStatus](
		ctx,
		c.client.Cache, c.cacheOption,
		api.GetPrefixedCacheKey("SampleJobRunTestStatus~", generator),
		generator.queryTestStatus,
		crtype.JobRunTestReportStatus{})

	if len(errs) > 0 {
		return nil, errs
	}

	return jobRunTestStatus.SampleStatus, nil
}

func (c *componentReportGenerator) getJobRunTestStatusFromBigQuery(ctx context.Context) (crtype.JobRunTestReportStatus, []error) {
	allJobVariants, errs := GetJobVariantsFromBigQuery(ctx, c.client, c.gcsBucket)
	if len(errs) > 0 {
		logrus.Errorf("failed to get variants from bigquery")
		return crtype.JobRunTestReportStatus{}, errs
	}
	var baseStatus, baseOverrideStatus, sampleStatus map[string][]crtype.JobRunTestStatusRow
	var baseErrs, baseOverrideErrs, sampleErrs []error
	wg := sync.WaitGroup{}

	if c.BaseOverrideRelease.Release != "" && c.BaseOverrideRelease.Release != c.BaseRelease.Release {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-ctx.Done():
				logrus.Infof("Context canceled while fetching base job run test status")
				return
			default:
				baseOverrideStatus, baseOverrideErrs = c.getBaseJobRunTestStatus(ctx, allJobVariants, c.BaseOverrideRelease.Release, c.BaseOverrideRelease.Start, c.BaseOverrideRelease.End)
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			logrus.Infof("Context canceled while fetching base job run test status")
			return
		default:
			baseStatus, baseErrs = c.getBaseJobRunTestStatus(ctx, allJobVariants, c.BaseRelease.Release, c.BaseRelease.Start, c.BaseRelease.End)
		}

	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			logrus.Infof("Context canceled while fetching sample job run test status")
			return
		default:
			sampleStatus, sampleErrs = c.getSampleJobRunTestStatus(ctx, allJobVariants)
		}

	}()
	wg.Wait()
	if len(baseErrs) != 0 || len(baseOverrideErrs) != 0 || len(sampleErrs) != 0 {
		errs = append(errs, baseErrs...)
		errs = append(errs, baseOverrideErrs...)
		errs = append(errs, sampleErrs...)
	}

	return crtype.JobRunTestReportStatus{BaseStatus: baseStatus, BaseOverrideStatus: baseOverrideStatus, SampleStatus: sampleStatus}, errs
}

// internalGenerateTestDetailsReport handles the report generation for the lowest level test report including
// breakdown by job as well as overall stats.
func (c *componentReportGenerator) internalGenerateTestDetailsReport(ctx context.Context,
	baseStatus map[string][]crtype.JobRunTestStatusRow,
	baseRelease string,
	baseStart,
	baseEnd *time.Time,
	sampleStatus map[string][]crtype.JobRunTestStatusRow) crtype.ReportTestDetails {
	result := crtype.ReportTestDetails{
		ReportTestIdentification: crtype.ReportTestIdentification{
			RowIdentification: crtype.RowIdentification{
				Component:  c.Component,
				Capability: c.Capability,
				TestID:     c.TestID,
			},
			ColumnIdentification: crtype.ColumnIdentification{
				Variants: c.RequestedVariants,
			},
		},
	}
	var resolvedIssueCompensation int
	approvedRegression := regressionallowances.IntentionalRegressionFor(c.SampleRelease.Release, result.ColumnIdentification, c.TestID)
	var baseRegression *regressionallowances.IntentionalRegression
	// if we are ignoring fallback then honor the settings for the baseRegression
	// otherwise let fallback determine the threshold
	if !c.IncludeMultiReleaseAnalysis {
		baseRegression = regressionallowances.IntentionalRegressionFor(baseRelease, result.ColumnIdentification, c.TestID)
	}
	// ignore triage if we have an intentional regression
	if approvedRegression == nil {
		resolvedIssueCompensation, _ = c.triagedIncidentsFor(ctx, result.ReportTestIdentification)
	}

	var totalBaseFailure, totalBaseSuccess, totalBaseFlake, totalSampleFailure, totalSampleSuccess, totalSampleFlake int
	var perJobBaseFailure, perJobBaseSuccess, perJobBaseFlake, perJobSampleFailure, perJobSampleSuccess, perJobSampleFlake int

	for prowJob, baseStatsList := range baseStatus {
		jobStats := crtype.TestDetailsJobStats{
			JobName: prowJob,
		}
		perJobBaseFailure = 0
		perJobBaseSuccess = 0
		perJobBaseFlake = 0
		perJobSampleFailure = 0
		perJobSampleSuccess = 0
		perJobSampleFlake = 0
		for _, baseStats := range baseStatsList {
			if result.JiraComponent == "" && baseStats.JiraComponent != "" {
				result.JiraComponent = baseStats.JiraComponent
			}
			if result.JiraComponentID == nil && baseStats.JiraComponentID != nil {
				result.JiraComponentID = baseStats.JiraComponentID
			}

			jobStats.BaseJobRunStats = append(jobStats.BaseJobRunStats, c.getJobRunStats(baseStats, c.prowURL, c.gcsBucket))
			perJobBaseSuccess += baseStats.SuccessCount
			perJobBaseFlake += baseStats.FlakeCount
			perJobBaseFailure += getFailureCount(baseStats)
		}
		if sampleStatsList, ok := sampleStatus[prowJob]; ok {
			for _, sampleStats := range sampleStatsList {
				if result.JiraComponent == "" && sampleStats.JiraComponent != "" {
					result.JiraComponent = sampleStats.JiraComponent
				}
				if result.JiraComponentID == nil && sampleStats.JiraComponentID != nil {
					result.JiraComponentID = sampleStats.JiraComponentID
				}

				jobStats.SampleJobRunStats = append(jobStats.SampleJobRunStats, c.getJobRunStats(sampleStats, c.prowURL, c.gcsBucket))
				perJobSampleSuccess += sampleStats.SuccessCount
				perJobSampleFlake += sampleStats.FlakeCount
				perJobSampleFailure += getFailureCount(sampleStats)
			}
			delete(sampleStatus, prowJob)
		}
		jobStats.BaseStats.SuccessCount = perJobBaseSuccess
		jobStats.BaseStats.FlakeCount = perJobBaseFlake
		jobStats.BaseStats.FailureCount = perJobBaseFailure
		jobStats.BaseStats.SuccessRate = c.getPassRate(perJobBaseSuccess, perJobBaseFailure, perJobBaseFlake)
		jobStats.SampleStats.SuccessCount = perJobSampleSuccess
		jobStats.SampleStats.FlakeCount = perJobSampleFlake
		jobStats.SampleStats.FailureCount = perJobSampleFailure
		jobStats.SampleStats.SuccessRate = c.getPassRate(perJobSampleSuccess, perJobSampleFailure, perJobSampleFlake)
		perceivedSampleFailure := perJobSampleFailure
		perceivedBaseFailure := perJobBaseFailure
		perceivedSampleSuccess := perJobSampleSuccess + perJobSampleFlake
		perceivedBaseSuccess := perJobBaseSuccess + perJobBaseFlake
		if c.FlakeAsFailure {
			perceivedSampleFailure = perJobSampleFailure + perJobSampleFlake
			perceivedBaseFailure = perJobBaseFailure + perJobBaseFlake
			perceivedSampleSuccess = perJobSampleSuccess
			perceivedBaseSuccess = perJobBaseSuccess
		}
		_, _, r, _ := fet.FisherExactTest(perceivedSampleFailure,
			perceivedSampleSuccess,
			perceivedBaseFailure,
			perceivedBaseSuccess)
		jobStats.Significant = r < 1-float64(c.Confidence)/100

		result.JobStats = append(result.JobStats, jobStats)

		totalBaseFailure += perJobBaseFailure
		totalBaseSuccess += perJobBaseSuccess
		totalBaseFlake += perJobBaseFlake
		totalSampleFailure += perJobSampleFailure
		totalSampleSuccess += perJobSampleSuccess
		totalSampleFlake += perJobSampleFlake
	}
	for prowJob, sampleStatsList := range sampleStatus {
		jobStats := crtype.TestDetailsJobStats{
			JobName: prowJob,
		}
		perJobSampleFailure = 0
		perJobSampleSuccess = 0
		perJobSampleFlake = 0
		for _, sampleStats := range sampleStatsList {
			jobStats.SampleJobRunStats = append(jobStats.SampleJobRunStats, c.getJobRunStats(sampleStats, c.prowURL, c.gcsBucket))
			perJobSampleSuccess += sampleStats.SuccessCount
			perJobSampleFlake += sampleStats.FlakeCount
			perJobSampleFailure += getFailureCount(sampleStats)
		}
		jobStats.SampleStats.SuccessCount = perJobSampleSuccess
		jobStats.SampleStats.FlakeCount = perJobSampleFlake
		jobStats.SampleStats.FailureCount = perJobSampleFailure
		jobStats.SampleStats.SuccessRate = c.getPassRate(perJobSampleSuccess, perJobSampleFailure, perJobSampleFlake)
		result.JobStats = append(result.JobStats, jobStats)
		perceivedSampleFailure := perJobSampleFailure
		perceivedSampleSuccess := perJobSampleSuccess + perJobSampleFlake
		if c.FlakeAsFailure {
			perceivedSampleFailure = perJobSampleFailure + perJobSampleFlake
			perceivedSampleSuccess = perJobSampleSuccess
		}
		_, _, r, _ := fet.FisherExactTest(perceivedSampleFailure,
			perceivedSampleSuccess,
			0,
			0)
		jobStats.Significant = r < 1-float64(c.Confidence)/100

		totalSampleFailure += perJobSampleFailure
		totalSampleSuccess += perJobSampleSuccess
		totalSampleFlake += perJobSampleFlake
	}
	sort.Slice(result.JobStats, func(i, j int) bool {
		return result.JobStats[i].JobName < result.JobStats[j].JobName
	})

	// The hope is that this goes away
	// once we agree we don't need to honor a higher intentional regression pass percentage
	if baseRegression != nil && baseRegression.PreviousPassPercentage(c.FlakeAsFailure) > c.getPassRate(totalBaseSuccess, totalBaseFailure, totalBaseFlake) {
		// override with  the basis regression previous values
		// testStats will reflect the expected threshold, not the computed values from the release with the allowed regression
		baseRegressionPreviousRelease, err := previousRelease(baseRelease)
		if err != nil {
			logrus.WithError(err).Error("Failed to determine the previous release for baseRegression")
		} else {
			totalBaseFlake = baseRegression.PreviousFlakes
			totalBaseSuccess = baseRegression.PreviousSuccesses
			totalBaseFailure = baseRegression.PreviousFailures
			baseRelease = baseRegressionPreviousRelease
			logrus.Infof("BaseRegression - PreviousPassPercentage overrides baseStats.  Release: %s, Successes: %d, Flakes: %d, Failures: %d", baseRelease, totalBaseSuccess, totalBaseFlake, totalBaseFailure)
		}
	}

	requiredConfidence := c.getRequiredConfidence(c.TestID, c.RequestedVariants)

	result.ReportTestStats = c.assessComponentStatus(
		requiredConfidence,
		totalSampleSuccess+totalSampleFailure+totalSampleFlake,
		totalSampleSuccess,
		totalSampleFlake,
		totalBaseSuccess+totalBaseFailure+totalBaseFlake,
		totalBaseSuccess,
		totalBaseFlake,
		approvedRegression,
		resolvedIssueCompensation,
		baseRelease,
		baseStart,
		baseEnd,
	)

	return result
}

func (c *componentReportGenerator) getJobRunStats(stats crtype.JobRunTestStatusRow, prowURL, gcsBucket string) crtype.TestDetailsJobRunStats {
	failure := getFailureCount(stats)
	url := fmt.Sprintf("%s/view/gs/%s/", prowURL, gcsBucket)
	subs := strings.Split(stats.FilePath, "/artifacts/")
	if len(subs) > 1 {
		url += subs[0]
	}
	jobRunStats := crtype.TestDetailsJobRunStats{
		TestStats: crtype.TestDetailsTestStats{
			SuccessRate:  c.getPassRate(stats.SuccessCount, failure, stats.FlakeCount),
			SuccessCount: stats.SuccessCount,
			FailureCount: failure,
			FlakeCount:   stats.FlakeCount,
		},
		JobURL: url,
	}
	return jobRunStats
}
