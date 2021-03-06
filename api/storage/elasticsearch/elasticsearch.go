package elasticsearch

import (
	"context"
	"encoding/json"
	"finala/api/config"
	"finala/api/storage"
	"reflect"
	"strconv"
	"strings"
	"time"

	elastic "github.com/olivere/elastic/v7"
	log "github.com/sirupsen/logrus"
)

// OrderedExecutionIDs will be the unmarshal response for ElasticSearch query  GetExecutions function
type orderedExecutionIDs struct {
	Buckets []struct {
		Key string `json:"key"`
	} `json:"buckets"`
}

const (
	// indexMapping define the default index mapping
	indexMapping = `{
		"mappings":{
			"properties":{
				"ResourceName":{
					"type":"keyword"
				},
				"ExecutionID":{
					"type":"keyword"
				},
				"EventType":{
					"type":"keyword"
				}
			}
		}
	}`
)

// StorageManager descrive elasticsearchStorage
type StorageManager struct {
	client       *elastic.Client
	defaultIndex string
}

// getESClient create new elasticsearch client
func getESClient(conf config.ElasticsearchConfig) (*elastic.Client, error) {

	client, err := elastic.NewClient(elastic.SetURL(strings.Join(conf.Endpoints, ",")),
		elastic.SetErrorLog(log.New()),
		//elastic.SetTraceLog(log.New()), // Uncomment for debugging ElasticSearch Queries
		elastic.SetBasicAuth(conf.Username, conf.Password),
		elastic.SetSniff(false),
		elastic.SetHealthcheck(true))

	return client, err

}

// NewStorageManager creates new elasticsearch storage
func NewStorageManager(conf config.ElasticsearchConfig) (*StorageManager, error) {

	var esclient *elastic.Client

	c := make(chan int, 1)
	go func() {
		var err error
		for {
			esclient, err = getESClient(conf)
			if err == nil {
				break
			}
			log.WithFields(log.Fields{
				"endpoint": conf.Endpoints,
			}).WithError(err).Warn("could not initialize connection to elasticsearch, retrying in 5 seconds")
			time.Sleep(5 * time.Second)
		}
		c <- 1
	}()

	select {
	case <-c:
	case <-time.After(60 * time.Second):
		log.Fatal("could not connect elasticsearch, timed out after 1 minute")
	}

	storageManager := &StorageManager{
		client:       esclient,
		defaultIndex: conf.Index,
	}

	return storageManager, nil
}

// Save new documents
func (sm *StorageManager) Save(data string) bool {

	_, err := sm.client.Index().
		Index(sm.defaultIndex).
		BodyJson(data).
		Do(context.Background())

	if err != nil {
		log.WithFields(log.Fields{
			"index": sm.defaultIndex,
			"data":  data,
		}).WithError(err).Error("Fail to save document")
		return false
	}

	return true

}

// getDynamicMatchQuery will iterate through a filters map and create Match Query for each of them
func (sm *StorageManager) getDynamicMatchQuery(filters map[string]string) []elastic.Query {
	dynamicMatchQuery := []elastic.Query{}
	for name, value := range filters {
		dynamicMatchQuery = append(dynamicMatchQuery, elastic.NewMatchQuery(name, value))
	}
	return dynamicMatchQuery
}

// GetSummary returns executions summary
func (sm *StorageManager) GetSummary(executionID string, filters map[string]string) (map[string]storage.CollectorsSummary, error) {
	summary := map[string]storage.CollectorsSummary{}
	executionIDQuery := elastic.NewMatchQuery("ExecutionID", executionID)
	eventTypeQuery := elastic.NewMatchQuery("EventType", "service_status")

	log.WithFields(log.Fields{
		"execution_id": executionIDQuery,
		"event_type":   eventTypeQuery,
	}).Debug("Going to get get summary with the following fields")

	searchResult, err := sm.client.Search().
		Query(elastic.NewBoolQuery().Must(eventTypeQuery, executionIDQuery)).
		Pretty(true).
		Size(100).
		Do(context.Background())

	if err != nil {
		log.WithError(err).Error("error when trying to get summary data")
		return summary, err
	}

	log.WithFields(log.Fields{
		"milliseconds": searchResult.TookInMillis,
		"hits":         len(searchResult.Hits.Hits),
	}).Debug("get all executions id response time")

	var summaryData storage.Summary
	for _, item := range searchResult.Each(reflect.TypeOf(summaryData)) {
		summaryRow, ok := item.(storage.Summary)
		if !ok {
			log.Error("could not parse summary row")
			continue
		}

		// check if the resource status already exists, if yes we check if have latest event
		val, found := summary[summaryRow.ResourceName]
		if found {
			if summaryRow.EventTime < val.EventTime {
				continue
			}
			delete(summary, summaryRow.ResourceName)
		}

		summary[summaryRow.ResourceName] = storage.CollectorsSummary{
			EventTime:    summaryRow.EventTime,
			Status:       summaryRow.Data.Status,
			ResourceName: summaryRow.ResourceName,
			ErrorMessage: summaryRow.Data.ErrorMessage,
		}
	}

	for resourceName, resourceData := range summary {
		filters["ResourceName"] = resourceName
		log.WithField("filters", filters).Debug("Going to get resources summary details with the following filters")
		totalSpent, resourceCount, err := sm.getResourceSummaryDetails(executionID, filters)

		if err != nil {
			continue
		}
		newResourceData := resourceData
		newResourceData.TotalSpent = totalSpent
		newResourceData.ResourceCount = resourceCount
		summary[resourceName] = newResourceData

	}

	return summary, nil

}

// getResourceSummaryDetails returns total resource spent and total resources detected
func (sm *StorageManager) getResourceSummaryDetails(executionID string, filters map[string]string) (float64, int64, error) {

	var totalSpent float64
	var resourceCount int64

	dynamicMatchQuery := sm.getDynamicMatchQuery(filters)
	dynamicMatchQuery = append(dynamicMatchQuery, elastic.NewMatchQuery("ExecutionID", executionID))
	dynamicMatchQuery = append(dynamicMatchQuery, elastic.NewMatchQuery("EventType", "resource_detected"))

	searchResult, err := sm.client.Search().
		Query(elastic.NewBoolQuery().Must(dynamicMatchQuery...)).
		Aggregation("sum", elastic.NewSumAggregation().Field("Data.PricePerMonth")).
		Size(0).Do(context.Background())

	if nil != err {
		log.WithError(err).WithFields(log.Fields{
			"filters":      filters,
			"milliseconds": searchResult.TookInMillis,
		}).Error("error when trying to get summary details")

		return totalSpent, resourceCount, err
	}

	log.WithFields(log.Fields{
		"filters":      filters,
		"milliseconds": searchResult.TookInMillis,
	}).Debug("get execution details")

	resp, ok := searchResult.Aggregations.Terms("sum")
	if ok {
		if val, ok := resp.Aggregations["value"]; ok {

			totalSpent, _ = strconv.ParseFloat(string(val), 64)
			resourceCount = searchResult.Hits.TotalHits.Value
		}
	}

	return totalSpent, resourceCount, nil
}

// GetExecutions returns collector executions
func (sm *StorageManager) GetExecutions(queryLimit int) ([]storage.Executions, error) {
	executions := []storage.Executions{}

	// First search for all message with eventType: service_status
	// Second look for message which have the field ExecutionID
	// Third Order the ExecutionID by EventTime Desc
	searchResult, err := sm.client.Search().Aggregation("orderedExecutionID", elastic.NewFiltersAggregation().
		Filters(elastic.NewBoolQuery().Filter(elastic.NewBoolQuery().Should(elastic.NewMatchQuery("EventType", "service_status")))).
		SubAggregation("ExecutionIDDesc", elastic.NewTermsAggregation().Field("ExecutionID.keyword").Size(queryLimit).Order("MaxEventTime", false).
			SubAggregation("MaxEventTime", elastic.NewMaxAggregation().Field("EventTime")))).
		Do(context.Background())

	if nil != err {
		log.WithError(err).WithFields(log.Fields{
			"milliseconds": searchResult.TookInMillis,
		}).Error("error when trying to get executions collectors")
		return executions, nil
	}

	resp, ok := searchResult.Aggregations.Terms("orderedExecutionID")
	if !ok {
		log.Error("orderedExecutionID field term does not exist")
		return executions, nil
	}

	for _, ExecutionIDBuckets := range resp.Buckets {
		descOrderedExecutionIDs := ExecutionIDBuckets.Aggregations["ExecutionIDDesc"]

		var executionsIDs orderedExecutionIDs
		err := json.Unmarshal([]byte(string(descOrderedExecutionIDs)), &executionsIDs)
		if err != nil {
			log.WithError(err).Error("error when trying to parse bucket aggregations execution ids")
		}

		for _, executionIDValue := range executionsIDs.Buckets {
			executionID := string(executionIDValue.Key)
			data := strings.Split(executionID, "_")

			// Remove the last element of Data which is the timestamp and leave all the others elements
			// Which construct the executionName
			executionName := strings.Join(data[:len(data)-1], "_")

			// Always take the last element which is the timestamp of the collector's run
			collectorExecutionTime, err := strconv.ParseInt(data[len(data)-1], 10, 64)
			if err != nil {
				log.WithError(err).WithField("collector_execution_time", collectorExecutionTime).Error("could not parse to int64")
				continue
			}

			executions = append(executions, storage.Executions{
				ID:   executionID,
				Name: executionName,
				Time: time.Unix(collectorExecutionTime, 0),
			})
		}
	}
	return executions, nil
}

// GetResources return resource data
func (sm *StorageManager) GetResources(resourceType string, executionID string) ([]map[string]interface{}, error) {

	var resources []map[string]interface{}
	componentQ := elastic.NewMatchQuery("EventType", "resource_detected")
	deploymentQ := elastic.NewMatchQuery("ExecutionID", executionID)
	ResourceNameQ := elastic.NewMatchQuery("ResourceName", resourceType)
	generalQ := elastic.NewBoolQuery()
	generalQ = generalQ.Must(componentQ).Must(deploymentQ).Must(ResourceNameQ)

	searchResult, err := sm.client.Search().
		Query(generalQ).
		Pretty(true).
		Size(100).
		Do(context.Background())

	if err != nil {
		log.WithError(err).Error("elasticsearch query error")
		return resources, err
	}

	for _, hit := range searchResult.Hits.Hits {

		rowData := make(map[string]interface{})
		err := json.Unmarshal([]byte(string(hit.Source)), &rowData)
		if err != nil {
			log.WithError(err).Error("error when trying to parse search result hits data")
		}

		resources = append(resources, rowData)
	}

	return resources, nil
}

// createIndex creating create elasticsearch index if not exists
func (sm *StorageManager) createIndex(index string) {

	exists, err := sm.client.IndexExists(index).Do(context.Background())
	if err != nil {
		log.WithFields(log.Fields{
			"index": index,
		}).WithError(err).Error("Error when trying to check if elasticsearch exists")
		return
	}
	if exists {
		log.WithField("index", index).Info("index already exists")
		return
	}

	ctx := context.Background()
	_, err = sm.client.CreateIndex(index).BodyString(indexMapping).Do(ctx)
	if err != nil {
		log.WithFields(log.Fields{
			"index": index,
		}).WithError(err).Error("Error when trying to create elasticsearch index")
	}

}
