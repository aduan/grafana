package azuremonitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/grafana/grafana/pkg/api/pluginproxy"
	"github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/plugins"
	"github.com/grafana/grafana/pkg/setting"
	opentracing "github.com/opentracing/opentracing-go"
	"golang.org/x/net/context/ctxhttp"

	"github.com/grafana/grafana/pkg/components/null"
	"github.com/grafana/grafana/pkg/components/simplejson"
	"github.com/grafana/grafana/pkg/tsdb"
)

// AzureMonitorDatasource calls the Azure Monitor API - one of the four API's supported
type AzureMonitorDatasource struct {
	httpClient *http.Client
	dsInfo     *models.DataSource
}

var (
	// 1m, 5m, 15m, 30m, 1h, 6h, 12h, 1d in milliseconds
	defaultAllowedIntervalsMS = []int64{60000, 300000, 900000, 1800000, 3600000, 21600000, 43200000, 86400000}
)

// executeTimeSeriesQuery does the following:
// 1. build the AzureMonitor url and querystring for each query
// 2. executes each query by calling the Azure Monitor API
// 3. parses the responses for each query into the timeseries format
func (e *AzureMonitorDatasource) executeTimeSeriesQuery(ctx context.Context, originalQueries []*tsdb.Query, timeRange *tsdb.TimeRange) (*tsdb.Response, error) {
	result := &tsdb.Response{
		Results: map[string]*tsdb.QueryResult{},
	}

	queries, err := e.buildQueries(ctx, originalQueries, timeRange)
	if err != nil {
		return nil, err
	}

	for _, query := range queries {
		queryRes, resp, err := e.executeQuery(ctx, query, originalQueries, timeRange)
		if err != nil {
			return nil, err
		}

		err = e.parseResponse(queryRes, resp, query)
		if err != nil {
			queryRes.Error = err
		}
		if val, ok := result.Results[query.RefID]; ok {
			val.Series = append(result.Results[query.RefID].Series, queryRes.Series...)
		} else {
			result.Results[query.RefID] = queryRes
		}
	}

	// Sort times series in evert query by name
	for _, query := range queries {
		sort.Slice(result.Results[query.RefID].Series, func(i, j int) bool {
			return result.Results[query.RefID].Series[i].Name < result.Results[query.RefID].Series[j].Name
		})
	}

	return result, nil
}

func (e *AzureMonitorDatasource) buildQueries(ctx context.Context, queries []*tsdb.Query, timeRange *tsdb.TimeRange) ([]*AzureMonitorQuery, error) {
	azureMonitorQueries := []*AzureMonitorQuery{}
	startTime, err := timeRange.ParseFrom()
	if err != nil {
		return nil, err
	}

	endTime, err := timeRange.ParseTo()
	if err != nil {
		return nil, err
	}

	for _, query := range queries {
		var azureMonitorTarget AzureMonitorQueryModel
		data, err := query.Model.Get("azureMonitor").MarshalJSON()
		if err != nil {
			return nil, fmt.Errorf("Invalid query format")
		}
		json.Unmarshal(data, &azureMonitorTarget)

		var azureMonitorData AzureMonitorData

		if azureMonitorTarget.QueryMode == "" {
			azureMonitorTarget.QueryMode = "singleResource"
			azureMonitorData = azureMonitorTarget.AzureMonitorData
		} else {
			azureMonitorData = azureMonitorTarget.Data[azureMonitorTarget.QueryMode]
		}

		if azureMonitorTarget.QueryMode == "singleResource" {
			azQuery, err := e.buildSingleQuery(query, &azureMonitorData, startTime, endTime, query.Model.Get("subscription").MustString())
			if err != nil {
				return nil, err
			}
			azureMonitorQueries = append(azureMonitorQueries, &azQuery)
		} else if azureMonitorTarget.QueryMode == "crossResource" {
			azQueries, err := e.buildMultipleResourcesQueries(ctx, query, &azureMonitorData, startTime, endTime)
			if err != nil {
				return nil, err
			}
			azureMonitorQueries = append(azureMonitorQueries, azQueries...)
		}

	}

	return azureMonitorQueries, nil
}

func (e *AzureMonitorDatasource) buildSingleQuery(query *tsdb.Query, azureMonitorData *AzureMonitorData, startTime time.Time, endTime time.Time, subscriptionID string) (AzureMonitorQuery, error) {
	var target string

	urlComponents := map[string]string{}
	urlComponents["subscription"] = subscriptionID
	urlComponents["resourceGroup"] = azureMonitorData.ResourceGroup
	urlComponents["metricDefinition"] = azureMonitorData.MetricDefinition
	urlComponents["resourceName"] = azureMonitorData.ResourceName

	ub := urlBuilder{
		DefaultSubscription: query.DataSource.JsonData.Get("subscriptionId").MustString(),
		Subscription:        urlComponents["subscription"],
		ResourceGroup:       urlComponents["resourceGroup"],
		MetricDefinition:    urlComponents["metricDefinition"],
		ResourceName:        urlComponents["resourceName"],
	}
	azureURL := ub.Build()

	timeGrain := azureMonitorData.TimeGrain
	timeGrains := azureMonitorData.AllowedTimeGrainsMs
	var err error
	if timeGrain == "auto" {
		timeGrain, err = e.setAutoTimeGrain(query.IntervalMs, timeGrains)
		if err != nil {
			return AzureMonitorQuery{}, err
		}
	}

	params := url.Values{}
	params.Add("api-version", "2018-01-01")
	params.Add("timespan", fmt.Sprintf("%v/%v", startTime.UTC().Format(time.RFC3339), endTime.UTC().Format(time.RFC3339)))
	params.Add("interval", timeGrain)
	params.Add("aggregation", azureMonitorData.Aggregation)
	params.Add("metricnames", azureMonitorData.MetricName)

	if azureMonitorData.MetricNamespace != "" {
		params.Add("metricnamespace", azureMonitorData.MetricNamespace)
	}

	dimension := strings.TrimSpace(azureMonitorData.Dimension)
	dimensionFilter := strings.TrimSpace(azureMonitorData.DimensionFilter)
	if len(dimension) > 0 && len(dimensionFilter) > 0 && dimension != "None" {
		params.Add("$filter", fmt.Sprintf("%s eq '%s'", dimension, dimensionFilter))
	}

	target = params.Encode()

	if setting.Env == setting.DEV {
		azlog.Debug("Azuremonitor request", "params", params)
	}

	return AzureMonitorQuery{
		URL:           azureURL,
		UrlComponents: urlComponents,
		Target:        target,
		Params:        params,
		RefID:         query.RefId,
		Alias:         azureMonitorData.Alias,
	}, nil
}

func (e *AzureMonitorDatasource) buildMultipleResourcesQueries(ctx context.Context, query *tsdb.Query, azureMonitorData *AzureMonitorData, startTime time.Time, endTime time.Time) ([]*AzureMonitorQuery, error) {
	azureMonitorQueries := []*AzureMonitorQuery{}
	subscriptions := query.Model.Get("subscriptions").MustArray()

	resources, err := e.getResources(ctx, azureMonitorData, subscriptions)
	if err != nil {
		return azureMonitorQueries, err
	}

	for _, resource := range resources {
		data := azureMonitorData
		data.ResourceGroup = resource.ParseGroup()
		data.MetricDefinition = resource.Type
		data.ResourceName = resource.Name

		azQuery, err := e.buildSingleQuery(query, data, startTime, endTime, resource.SubscriptionID)
		if err != nil {
			return nil, err
		}
		azureMonitorQueries = append(azureMonitorQueries, &azQuery)
	}

	return azureMonitorQueries, nil
}

func (e *AzureMonitorDatasource) getResources(ctx context.Context, azureMonitorData *AzureMonitorData, subscriptions []interface{}) ([]resource, error) {
	resourcesMap := map[string]resource{}

	for _, subscriptionID := range subscriptions {
		resourcesResponse, err := e.executeResourcesQuery(ctx, fmt.Sprintf("%v", subscriptionID))
		if err != nil {
			return []resource{}, err
		}

		for _, resourceResponse := range resourcesResponse.Value {
			resource := resource{
				ID:             resourceResponse.ID,
				Name:           resourceResponse.Name,
				Type:           resourceResponse.Type,
				Location:       resourceResponse.Location,
				SubscriptionID: fmt.Sprintf("%v", subscriptionID),
			}

			match := contains(azureMonitorData.ResourceGroups, resource.ParseGroup()) &&
				contains(azureMonitorData.Locations, resource.Location) &&
				azureMonitorData.MetricDefinition == resource.Type

			if _, ok := resourcesMap[resource.GetKey()]; !ok && match {
				resourcesMap[resource.GetKey()] = resource
			}
		}
	}

	resources := []resource{}
	for _, resource := range resourcesMap {
		resources = append(resources, resource)
	}

	return resources, nil
}

// setAutoTimeGrain tries to find the closest interval to the query's intervalMs value
// if the metric has a limited set of possible intervals/time grains then use those
// instead of the default list of intervals
func (e *AzureMonitorDatasource) setAutoTimeGrain(intervalMs int64, timeGrains []int64) (string, error) {

	autoInterval := e.findClosestAllowedIntervalMS(intervalMs, timeGrains)
	tg := &TimeGrain{}
	autoTimeGrain, err := tg.createISO8601DurationFromIntervalMS(autoInterval)
	if err != nil {
		return "", err
	}

	return autoTimeGrain, nil
}

func (e *AzureMonitorDatasource) executeQuery(ctx context.Context, query *AzureMonitorQuery, queries []*tsdb.Query, timeRange *tsdb.TimeRange) (*tsdb.QueryResult, AzureMonitorResponse, error) {
	queryResult := &tsdb.QueryResult{Meta: simplejson.New(), RefId: query.RefID}

	req, err := e.createRequest(ctx, e.dsInfo)
	if err != nil {
		queryResult.Error = err
		return queryResult, AzureMonitorResponse{}, nil
	}

	req.URL.Path = path.Join(req.URL.Path, query.URL)
	req.URL.RawQuery = query.Params.Encode()
	queryResult.Meta.Set("rawQuery", req.URL.RawQuery)

	span, ctx := opentracing.StartSpanFromContext(ctx, "azuremonitor query")
	span.SetTag("target", query.Target)
	span.SetTag("from", timeRange.From)
	span.SetTag("until", timeRange.To)
	span.SetTag("datasource_id", e.dsInfo.Id)
	span.SetTag("org_id", e.dsInfo.OrgId)

	defer span.Finish()

	opentracing.GlobalTracer().Inject(
		span.Context(),
		opentracing.HTTPHeaders,
		opentracing.HTTPHeadersCarrier(req.Header))

	azlog.Debug("AzureMonitor", "Request URL", req.URL.String())
	res, err := ctxhttp.Do(ctx, e.httpClient, req)
	if err != nil {
		queryResult.Error = err
		return queryResult, AzureMonitorResponse{}, nil
	}

	data, err := e.unmarshalResponse(res)
	if err != nil {
		queryResult.Error = err
		return queryResult, AzureMonitorResponse{}, nil
	}

	return queryResult, data, nil
}

func (e *AzureMonitorDatasource) executeResourcesQuery(ctx context.Context, subscriptionID string) (ResourcesResponse, error) {
	req, err := e.createRequest(ctx, e.dsInfo)
	if err != nil {
		return ResourcesResponse{}, err
	}

	params := url.Values{}
	params.Add("api-version", "2018-01-01")
	req.URL.Path = path.Join(req.URL.Path, subscriptionID, "resources")
	req.URL.RawQuery = params.Encode()

	res, err := ctxhttp.Do(ctx, e.httpClient, req)
	if err != nil {
		return ResourcesResponse{}, err
	}
	data, err := e.unmarshalResourcesResponse(res)
	if err != nil {
		return ResourcesResponse{}, err
	}

	return data, nil
}

func (e *AzureMonitorDatasource) createRequest(ctx context.Context, dsInfo *models.DataSource) (*http.Request, error) {
	// find plugin
	plugin, ok := plugins.DataSources[dsInfo.Type]
	if !ok {
		return nil, errors.New("Unable to find datasource plugin Azure Monitor")
	}

	var azureMonitorRoute *plugins.AppPluginRoute
	for _, route := range plugin.Routes {
		if route.Path == "azuremonitor" {
			azureMonitorRoute = route
			break
		}
	}

	cloudName := dsInfo.JsonData.Get("cloudName").MustString("azuremonitor")
	proxyPass := fmt.Sprintf("%s/subscriptions", cloudName)

	u, _ := url.Parse(dsInfo.Url)
	u.Path = path.Join(u.Path, "render")

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		azlog.Error("Failed to create request", "error", err)
		return nil, fmt.Errorf("Failed to create request. error: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", fmt.Sprintf("Grafana/%s", setting.BuildVersion))

	pluginproxy.ApplyRoute(ctx, req, proxyPass, azureMonitorRoute, dsInfo)
	return req, nil
}

func (e *AzureMonitorDatasource) unmarshalResponse(res *http.Response) (AzureMonitorResponse, error) {
	body, err := ioutil.ReadAll(res.Body)
	defer res.Body.Close()
	if err != nil {
		return AzureMonitorResponse{}, err
	}

	if res.StatusCode/100 != 2 {
		azlog.Error("Request failed", "status", res.Status, "body", string(body))
		return AzureMonitorResponse{}, fmt.Errorf(string(body))
	}

	var data AzureMonitorResponse
	err = json.Unmarshal(body, &data)
	if err != nil {
		azlog.Error("Failed to unmarshal AzureMonitor response", "error", err, "status", res.Status, "body", string(body))
		return AzureMonitorResponse{}, err
	}

	return data, nil
}

func (e *AzureMonitorDatasource) unmarshalResourcesResponse(res *http.Response) (ResourcesResponse, error) {
	body, err := ioutil.ReadAll(res.Body)
	defer res.Body.Close()
	if err != nil {
		return ResourcesResponse{}, err
	}

	if res.StatusCode/100 != 2 {
		azlog.Error("Request failed", "status", res.Status, "body", string(body))
		return ResourcesResponse{}, fmt.Errorf(string(body))
	}

	var data ResourcesResponse
	err = json.Unmarshal(body, &data)
	if err != nil {
		azlog.Error("Failed to unmarshal AzureMonitor Resource response", "error", err, "status", res.Status, "body", string(body))
		return ResourcesResponse{}, err
	}

	return data, nil
}

func (e *AzureMonitorDatasource) parseResponse(queryRes *tsdb.QueryResult, data AzureMonitorResponse, query *AzureMonitorQuery) error {
	if len(data.Value) == 0 {
		return nil
	}

	for _, series := range data.Value[0].Timeseries {
		points := []tsdb.TimePoint{}

		metadataName := ""
		metadataValue := ""
		if len(series.Metadatavalues) > 0 {
			metadataName = series.Metadatavalues[0].Name.LocalizedValue
			metadataValue = series.Metadatavalues[0].Value
		}
		metricName := formatLegendKey(query.Alias, query.UrlComponents["resourceName"], data.Value[0].Name.LocalizedValue, metadataName, metadataValue, data.Namespace, data.Value[0].ID)

		for _, point := range series.Data {
			var value float64
			switch query.Params.Get("aggregation") {
			case "Average":
				value = point.Average
			case "Total":
				value = point.Total
			case "Maximum":
				value = point.Maximum
			case "Minimum":
				value = point.Minimum
			case "Count":
				value = point.Count
			default:
				value = point.Count
			}
			points = append(points, tsdb.NewTimePoint(null.FloatFrom(value), float64((point.TimeStamp).Unix())*1000))
		}

		queryRes.Series = append(queryRes.Series, &tsdb.TimeSeries{
			Name:   metricName,
			Points: points,
		})
	}
	queryRes.Meta.Set("unit", data.Value[0].Unit)

	return nil
}

// findClosestAllowedIntervalMs is used for the auto time grain setting.
// It finds the closest time grain from the list of allowed time grains for Azure Monitor
// using the Grafana interval in milliseconds
// Some metrics only allow a limited list of time grains. The allowedTimeGrains parameter
// allows overriding the default list of allowed time grains.
func (e *AzureMonitorDatasource) findClosestAllowedIntervalMS(intervalMs int64, allowedTimeGrains []int64) int64 {
	allowedIntervals := defaultAllowedIntervalsMS

	if len(allowedTimeGrains) > 0 {
		allowedIntervals = allowedTimeGrains
	}

	closest := allowedIntervals[0]

	for i, allowed := range allowedIntervals {
		if intervalMs > allowed {
			if i+1 < len(allowedIntervals) {
				closest = allowedIntervals[i+1]
			} else {
				closest = allowed
			}
		}
	}
	return closest
}

// formatLegendKey builds the legend key or timeseries name
// Alias patterns like {{resourcename}} are replaced with the appropriate data values.
func formatLegendKey(alias string, resourceName string, metricName string, metadataName string, metadataValue string, namespace string, seriesID string) string {
	if alias == "" {
		if len(metadataName) > 0 {
			return fmt.Sprintf("%s{%s=%s}.%s", resourceName, metadataName, metadataValue, metricName)
		}
		return fmt.Sprintf("%s.%s", resourceName, metricName)
	}

	startIndex := strings.Index(seriesID, "/resourceGroups/") + 16
	endIndex := strings.Index(seriesID, "/providers")
	resourceGroup := seriesID[startIndex:endIndex]

	result := legendKeyFormat.ReplaceAllFunc([]byte(alias), func(in []byte) []byte {
		metaPartName := strings.Replace(string(in), "{{", "", 1)
		metaPartName = strings.Replace(metaPartName, "}}", "", 1)
		metaPartName = strings.ToLower(strings.TrimSpace(metaPartName))

		if metaPartName == "resourcegroup" {
			return []byte(resourceGroup)
		}

		if metaPartName == "namespace" {
			return []byte(namespace)
		}

		if metaPartName == "resourcen	ame" {
			return []byte(resourceName)
		}

		if metaPartName == "metric" {
			return []byte(metricName)
		}

		if metaPartName == "dimensionname" {
			return []byte(metadataName)
		}

		if metaPartName == "dimensionvalue" {
			return []byte(metadataValue)
		}

		return in
	})

	return string(result)
}

func contains(a []string, x string) bool {
	for _, n := range a {
		if x == n {
			return true
		}
	}
	return false
}
