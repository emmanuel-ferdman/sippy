package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	gosort "sort"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"

	apitype "github.com/openshift/sippy/pkg/apis/api"
	"github.com/openshift/sippy/pkg/db"
	"github.com/openshift/sippy/pkg/db/query"
	"github.com/openshift/sippy/pkg/filter"
	"github.com/openshift/sippy/pkg/html/installhtml"
	"github.com/openshift/sippy/pkg/util/param"
)

const (
	testReport7dMatView          = "prow_test_report_7d_matview"
	testReport2dMatView          = "prow_test_report_2d_matview"
	payloadFailedTests14dMatView = "payload_test_failures_14d_matview"
)

func PrintTestsDetailsJSONFromDB(w http.ResponseWriter, release string, testSubstrings []string, dbc *db.DB) {
	responseStr, err := installhtml.TestDetailTestsFromDB(dbc, release, testSubstrings)
	if err != nil {
		RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{"code": http.StatusBadRequest, "message": err.Error()})
		return
	}
	RespondWithJSON(http.StatusOK, w, responseStr)
}

func GetTestOutputsFromDB(dbc *db.DB, release, test string, filters *filter.Filter, quantity int) ([]apitype.TestOutput, error) {
	var includedVariants, excludedVariants []string
	if filters != nil {
		for _, f := range filters.Items {
			if f.Field == "variants" {
				if f.Not {
					excludedVariants = append(excludedVariants, f.Value)
				} else {
					includedVariants = append(includedVariants, f.Value)
				}
			}
		}
	}

	return query.TestOutputs(dbc, release, test, includedVariants, excludedVariants, quantity)
}

func GetTestDurationsFromDB(dbc *db.DB, release, test string, filters *filter.Filter) (map[string]float64, error) {
	var includedVariants, excludedVariants []string
	if filters != nil {
		for _, f := range filters.Items {
			if f.Field == "variants" {
				if f.Not {
					excludedVariants = append(excludedVariants, f.Value)
				} else {
					includedVariants = append(includedVariants, f.Value)
				}
			}
		}
	}

	return query.TestDurations(dbc, release, test, includedVariants, excludedVariants)
}

type testsAPIResult []apitype.Test

func (tests testsAPIResult) sort(req *http.Request) testsAPIResult {
	sortField := param.SafeRead(req, "sortField")
	sort := param.SafeRead(req, "sort")

	if sortField == "" {
		sortField = "current_pass_percentage"
	}

	if sort == "" {
		sort = "asc"
	}

	gosort.Slice(tests, func(i, j int) bool {
		if sort == "asc" {
			return filter.Compare(tests[i], tests[j], sortField)
		}
		return filter.Compare(tests[j], tests[i], sortField)
	})

	return tests
}

func (tests testsAPIResult) limit(req *http.Request) testsAPIResult {
	limit, _ := strconv.Atoi(req.URL.Query().Get("limit"))
	if limit == 0 || len(tests) < limit {
		return tests
	}

	return tests[:limit]
}

func PrintTestsJSONFromDB(release string, w http.ResponseWriter, req *http.Request, dbc *db.DB) {
	var fil *filter.Filter

	// Collapse means to produce an aggregated test result of all variant (NURP+ - network, upgrade, release, platform)
	// combos. Uncollapsed results shows you the per-NURP+ result for each test (currently approx. 50,000 rows: filtering
	// is advised)
	collapseStr := req.URL.Query().Get("collapse")
	collapse := true
	if collapseStr == "false" {
		collapse = false
	}

	overallStr := req.URL.Query().Get("overall")
	includeOverall := !collapse
	if overallStr != "" {
		includeOverall, _ = strconv.ParseBool(overallStr)
	}

	queryFilter := req.URL.Query().Get("filter")
	if queryFilter != "" {
		fil = &filter.Filter{}
		if err := json.Unmarshal([]byte(queryFilter), fil); err != nil {
			RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{"code": http.StatusBadRequest, "message": "Could not marshal query:" + err.Error()})
			return
		}
	}

	// If requesting a two day report, we make the comparison between the last
	// period (typically 7 days) and the last two days.
	period := req.URL.Query().Get("period")
	if period != "" && period != "default" && period != "current" && period != "twoDay" {
		RespondWithJSON(http.StatusBadRequest, w, map[string]interface{}{"code": http.StatusBadRequest, "message": "Unknown period"})
		return
	}

	testsResult, overall, err := BuildTestsResults(dbc, release, period, collapse, includeOverall, fil)
	if err != nil {
		RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{"code": http.StatusInternalServerError, "message": "Error building job report:" + err.Error()})
		return
	}

	testsResult = testsResult.sort(req).limit(req)
	if overall != nil {
		testsResult = append([]apitype.Test{*overall}, testsResult...)
	}

	RespondWithJSON(http.StatusOK, w, testsResult)
}

func PrintCanaryTestsFromDB(release string, w http.ResponseWriter, dbc *db.DB) {
	f := filter.Filter{
		Items: []filter.FilterItem{
			{
				Field:    "current_pass_percentage",
				Operator: ">=",
				Value:    "99",
			},
		},
	}

	results, _, err := BuildTestsResults(dbc, release, "default", true, false, &f)
	if err != nil {
		RespondWithJSON(http.StatusInternalServerError, w, map[string]interface{}{"code": http.StatusInternalServerError, "message": "Error building test report:" + err.Error()})
		return
	}

	w.Header().Set("Content-Type", "text/plain;charset=UTF-8")
	for _, result := range results {
		fmt.Fprintf(w, "%q:struct{}{},\n", result.Name)
	}
}

func BuildTestsResults(dbc *db.DB, release, period string, collapse, includeOverall bool, fil *filter.Filter) (testsAPIResult, *apitype.Test, error) { //lint:ignore
	now := time.Now()

	// Test results are generated by using two subqueries, which need to be filtered separately. Once during
	// pre-processing where we're evaluating summed variant results, and in post-processing after we've
	// assembled our final temporary table.
	var rawFilter, processedFilter *filter.Filter
	if fil != nil {
		rawFilter, processedFilter = fil.Split([]string{"name", "variants"})
	}

	table := testReport7dMatView
	if period == "twoDay" {
		table = testReport2dMatView
	}

	rawQuery := dbc.DB.
		Table(table).
		Where("release = ?", release)

	// Collapse groups the test results together -- otherwise we return the test results per-variant combo (NURP+)
	variantSelect := ""
	if collapse {
		rawQuery = rawQuery.Select(`name,jira_component,jira_component_id,` + query.QueryTestSummer).Group("name,jira_component,jira_component_id")
	} else {
		rawQuery = query.TestsByNURPAndStandardDeviation(dbc, release, table)
		variantSelect = "suite_name, variants," +
			"delta_from_working_average, working_average, working_standard_deviation, " +
			"delta_from_passing_average, passing_average, passing_standard_deviation, " +
			"delta_from_flake_average, flake_average, flake_standard_deviation, "

	}

	if rawFilter != nil {
		rawQuery = rawFilter.ToSQL(rawQuery, apitype.Test{})
	}

	testReports := make([]apitype.Test, 0)
	// FIXME: Add test id to matview, for now generate with ROW_NUMBER OVER
	processedResults := dbc.DB.Table("(?) as results", rawQuery).
		Select(`ROW_NUMBER() OVER() as id, name, jira_component, jira_component_id,` + variantSelect + query.QueryTestSummarizer).
		Where("current_runs > 0 or previous_runs > 0")

	finalResults := dbc.DB.Table("(?) as final_results", processedResults)
	if processedFilter != nil {
		finalResults = processedFilter.ToSQL(finalResults, apitype.Test{})
	}

	frr := finalResults.Scan(&testReports)
	if frr.Error != nil {
		log.WithError(finalResults.Error).Error("error querying test reports")
		return []apitype.Test{}, nil, frr.Error
	}

	// Produce a special "overall" test that has a summary of all the selected tests.
	var overallTest *apitype.Test
	if includeOverall {
		finalResults := dbc.DB.Table("(?) as final_results", finalResults)
		finalResults = finalResults.Select(query.QueryTestSummer)
		summaryResult := dbc.DB.Table("(?) as overall", finalResults).Select(query.QueryTestSummarizer)
		overallTest = &apitype.Test{
			ID:   math.MaxInt32,
			Name: "Overall",
		}
		// TODO: column open_bugs does not exist here?
		summaryResult.Scan(overallTest)
	}

	elapsed := time.Since(now)
	log.WithFields(log.Fields{
		"elapsed": elapsed,
		"reports": len(testReports),
	}).Debug("BuildTestsResults completed")

	return testReports, overallTest, nil
}
