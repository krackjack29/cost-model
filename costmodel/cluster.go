package costmodel

import (
	"fmt"
	"os"
	"time"

	costAnalyzerCloud "github.com/kubecost/cost-model/cloud"
	prometheusClient "github.com/prometheus/client_golang/api"

	"k8s.io/klog"
)

const (
	queryClusterCores = `sum(
		avg(avg_over_time(kube_node_status_capacity_cpu_cores[%s] %s)) by (node, cluster_id) * avg(avg_over_time(node_cpu_hourly_cost[%s] %s)) by (node, cluster_id) * 730 +
		avg(avg_over_time(node_gpu_hourly_cost[%s] %s)) by (node, cluster_id) * 730
	  ) by (cluster_id)`

	queryClusterRAM = `sum(
		avg(avg_over_time(kube_node_status_capacity_memory_bytes[%s] %s)) by (node, cluster_id) / 1024 / 1024 / 1024 * avg(avg_over_time(node_ram_hourly_cost[%s] %s)) by (node, cluster_id) * 730
	  ) by (cluster_id)`

	queryStorage = `sum(
		avg(avg_over_time(pv_hourly_cost[%s] %s)) by (persistentvolume, cluster_id) * 730 
		* avg(avg_over_time(kube_persistentvolume_capacity_bytes[%s] %s)) by (persistentvolume, cluster_id) / 1024 / 1024 / 1024
	  ) by (cluster_id) %s`

	queryTotal = `sum(avg(node_total_hourly_cost) by (node, cluster_id)) * 730 +
	  sum(
		avg(avg_over_time(pv_hourly_cost[1h])) by (persistentvolume, cluster_id) * 730 
		* avg(avg_over_time(kube_persistentvolume_capacity_bytes[1h])) by (persistentvolume, cluster_id) / 1024 / 1024 / 1024
	  ) by (cluster_id) %s`
)

type Totals struct {
	TotalCost   [][]string `json:"totalcost"`
	CPUCost     [][]string `json:"cpucost"`
	MemCost     [][]string `json:"memcost"`
	StorageCost [][]string `json:"storageCost"`
}

func resultToTotals(qr interface{}) ([][]string, error) {
	results, err := NewQueryResults(qr)
	if err != nil {
		return nil, err
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("Not enough data available in the selected time range")
	}

	result := results[0]
	totals := [][]string{}
	for _, value := range result.Values {
		d0 := fmt.Sprintf("%f", value.Timestamp)
		d1 := fmt.Sprintf("%f", value.Value)
		toAppend := []string{
			d0,
			d1,
		}
		totals = append(totals, toAppend)
	}
	return totals, nil
}

func resultToTotal(qr interface{}) (map[string][][]string, error) {
	defaultClusterID := os.Getenv(clusterIDKey)

	results, err := NewQueryResults(qr)
	if err != nil {
		return nil, err
	}

	toReturn := make(map[string][][]string)
	for _, result := range results {
		clusterID, _ := result.GetString("cluster_id")
		if clusterID == "" {
			clusterID = defaultClusterID
		}

		// Expect a single value only
		if len(result.Values) == 0 {
			klog.V(1).Infof("[Warning] Metric values did not contain any valid data.")
			continue
		}

		value := result.Values[0]
		d0 := fmt.Sprintf("%f", value.Timestamp)
		d1 := fmt.Sprintf("%f", value.Value)
		toAppend := []string{
			d0,
			d1,
		}
		if t, ok := toReturn[clusterID]; ok {
			t = append(t, toAppend)
		} else {
			toReturn[clusterID] = [][]string{toAppend}
		}
	}

	return toReturn, nil
}

// ClusterCostsForAllClusters gives the cluster costs averaged over a window of time for all clusters.
func ClusterCostsForAllClusters(cli prometheusClient.Client, cloud costAnalyzerCloud.Provider, windowString, offset string) (map[string]*Totals, error) {

	if offset != "" {
		offset = fmt.Sprintf("offset %s", offset)
	}

	localStorageQuery, err := cloud.GetLocalStorageQuery(offset)
	if err != nil {
		return nil, err
	}
	if localStorageQuery != "" {
		localStorageQuery = fmt.Sprintf("+ %s", localStorageQuery)
	}

	qCores := fmt.Sprintf(queryClusterCores, windowString, offset, windowString, offset, windowString, offset)
	qRAM := fmt.Sprintf(queryClusterRAM, windowString, offset, windowString, offset)
	qStorage := fmt.Sprintf(queryStorage, windowString, offset, windowString, offset, localStorageQuery)

	klog.V(4).Infof("Running query %s", qCores)
	resultClusterCores, err := Query(cli, qCores)
	if err != nil {
		return nil, fmt.Errorf("Error for query %s: %s", qCores, err.Error())
	}
	klog.V(4).Infof("Running query %s", qRAM)
	resultClusterRAM, err := Query(cli, qRAM)
	if err != nil {
		return nil, fmt.Errorf("Error for query %s: %s", qRAM, err.Error())
	}
	klog.V(4).Infof("Running query %s", qRAM)
	resultStorage, err := Query(cli, qStorage)
	if err != nil {
		return nil, fmt.Errorf("Error for query %s: %s", qStorage, err.Error())
	}

	toReturn := make(map[string]*Totals)

	coreTotal, err := resultToTotal(resultClusterCores)
	if err != nil {
		return nil, fmt.Errorf("Error for query %s: %s", qCores, err.Error())
	}
	for clusterID, total := range coreTotal {
		if _, ok := toReturn[clusterID]; !ok {
			toReturn[clusterID] = &Totals{}
		}
		toReturn[clusterID].CPUCost = total
	}

	ramTotal, err := resultToTotal(resultClusterRAM)
	if err != nil {
		return nil, fmt.Errorf("Error for query %s: %s", qRAM, err.Error())
	}
	for clusterID, total := range ramTotal {
		if _, ok := toReturn[clusterID]; !ok {
			toReturn[clusterID] = &Totals{}
		}
		toReturn[clusterID].MemCost = total
	}

	storageTotal, err := resultToTotal(resultStorage)
	if err != nil {
		return nil, fmt.Errorf("Error for query %s: %s", qStorage, err.Error())
	}
	for clusterID, total := range storageTotal {
		if _, ok := toReturn[clusterID]; !ok {
			toReturn[clusterID] = &Totals{}
		}
		toReturn[clusterID].StorageCost = total
	}

	return toReturn, nil
}

// ClusterCosts gives the current full cluster costs averaged over a window of time.
func ClusterCosts(cli prometheusClient.Client, cloud costAnalyzerCloud.Provider, windowString, offset string) (*Totals, error) {

	// turn offsets of the format "[0-9+]h" into the format "offset [0-9+]h" for use in query templatess
	if offset != "" {
		offset = fmt.Sprintf("offset %s", offset)
	}

	localStorageQuery, err := cloud.GetLocalStorageQuery(offset)
	if err != nil {
		return nil, err
	}
	if localStorageQuery != "" {
		localStorageQuery = fmt.Sprintf("+ %s", localStorageQuery)
	}

	qCores := fmt.Sprintf(queryClusterCores, windowString, offset, windowString, offset, windowString, offset)
	qRAM := fmt.Sprintf(queryClusterRAM, windowString, offset, windowString, offset)
	qStorage := fmt.Sprintf(queryStorage, windowString, offset, windowString, offset, localStorageQuery)
	qTotal := fmt.Sprintf(queryTotal, localStorageQuery)

	resultClusterCores, err := Query(cli, qCores)
	if err != nil {
		return nil, err
	}
	resultClusterRAM, err := Query(cli, qRAM)
	if err != nil {
		return nil, err
	}

	resultStorage, err := Query(cli, qStorage)
	if err != nil {
		return nil, err
	}

	resultTotal, err := Query(cli, qTotal)
	if err != nil {
		return nil, err
	}

	coreTotal, err := resultToTotal(resultClusterCores)
	if err != nil {
		return nil, err
	}

	ramTotal, err := resultToTotal(resultClusterRAM)
	if err != nil {
		return nil, err
	}

	storageTotal, err := resultToTotal(resultStorage)
	if err != nil {
		return nil, err
	}

	clusterTotal, err := resultToTotal(resultTotal)
	if err != nil {
		return nil, err
	}

	defaultClusterID := os.Getenv(clusterIDKey)

	return &Totals{
		TotalCost:   clusterTotal[defaultClusterID],
		CPUCost:     coreTotal[defaultClusterID],
		MemCost:     ramTotal[defaultClusterID],
		StorageCost: storageTotal[defaultClusterID],
	}, nil
}

// ClusterCostsOverTime gives the full cluster costs over time
func ClusterCostsOverTime(cli prometheusClient.Client, cloud costAnalyzerCloud.Provider, startString, endString, windowString, offset string) (*Totals, error) {

	localStorageQuery, err := cloud.GetLocalStorageQuery(offset)
	if err != nil {
		return nil, err
	}
	if localStorageQuery != "" {
		localStorageQuery = fmt.Sprintf("+ %s", localStorageQuery)
	}

	layout := "2006-01-02T15:04:05.000Z"

	start, err := time.Parse(layout, startString)
	if err != nil {
		klog.V(1).Infof("Error parsing time " + startString + ". Error: " + err.Error())
		return nil, err
	}
	end, err := time.Parse(layout, endString)
	if err != nil {
		klog.V(1).Infof("Error parsing time " + endString + ". Error: " + err.Error())
		return nil, err
	}
	window, err := time.ParseDuration(windowString)
	if err != nil {
		klog.V(1).Infof("Error parsing time " + windowString + ". Error: " + err.Error())
		return nil, err
	}

	// turn offsets of the format "[0-9+]h" into the format "offset [0-9+]h" for use in query templatess
	if offset != "" {
		offset = fmt.Sprintf("offset %s", offset)
	}

	qCores := fmt.Sprintf(queryClusterCores, windowString, offset, windowString, offset, windowString, offset)
	qRAM := fmt.Sprintf(queryClusterRAM, windowString, offset, windowString, offset)
	qStorage := fmt.Sprintf(queryStorage, windowString, offset, windowString, offset, localStorageQuery)
	qTotal := fmt.Sprintf(queryTotal, localStorageQuery)

	resultClusterCores, err := QueryRange(cli, qCores, start, end, window)
	if err != nil {
		return nil, err
	}
	resultClusterRAM, err := QueryRange(cli, qRAM, start, end, window)
	if err != nil {
		return nil, err
	}

	resultStorage, err := QueryRange(cli, qStorage, start, end, window)
	if err != nil {
		return nil, err
	}

	resultTotal, err := QueryRange(cli, qTotal, start, end, window)
	if err != nil {
		return nil, err
	}

	coreTotal, err := resultToTotals(resultClusterCores)
	if err != nil {
		return nil, err
	}

	ramTotal, err := resultToTotals(resultClusterRAM)
	if err != nil {
		return nil, err
	}

	storageTotal, err := resultToTotals(resultStorage)
	if err != nil {
		return nil, err
	}

	clusterTotal, err := resultToTotals(resultTotal)
	if err != nil {
		return nil, err
	}

	return &Totals{
		TotalCost:   clusterTotal,
		CPUCost:     coreTotal,
		MemCost:     ramTotal,
		StorageCost: storageTotal,
	}, nil

}
