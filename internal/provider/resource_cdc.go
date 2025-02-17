package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/datastax/astra-client-go/v2/astra"
	astrastreaming "github.com/datastax/astra-client-go/v2/astra-streaming"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
)

func resourceCDC() *schema.Resource {
	return &schema.Resource{
		Description:   "`astra_cdc` enables cdc for an Astra Serverless table.",
		CreateContext: resourceCDCCreate,
		ReadContext:   resourceCDCRead,
		DeleteContext: resourceCDCDelete,

		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},

		Schema: map[string]*schema.Schema{
			// Required
			"table": {
				Description:  "Astra database table.",
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validation.StringMatch(regexp.MustCompile("^.{2,}"), "name must be atleast 2 characters"),
			},
			"keyspace": {
				Description:      "Initial keyspace name. For additional keyspaces, use the astra_keyspace resource.",
				Type:             schema.TypeString,
				Required:         true,
				ForceNew:         true,
				ValidateDiagFunc: validateKeyspace,
			},
			"database_id": {
				Description:  "Astra database to create the keyspace.",
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validation.IsUUID,
			},
			"database_name": {
				Description: "Astra database name.",
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
			},
			"topic_partitions": {
				Description: "Number of partitions in cdc topic.",
				Type:        schema.TypeInt,
				Required:    true,
				ForceNew:    true,
			},
			"tenant_name": {
				Description: "Streaming tenant name",
				Type:        schema.TypeString,
				Required:    true,
				ForceNew:    true,
			},
			"connector_status": {
				Description: "Connector Status",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"data_topic": {
				Description: "Data topic name",
				Type:        schema.TypeString,
				Computed:    true,
			},
		},
	}
}

func resourceCDCDelete(ctx context.Context, resourceData *schema.ResourceData, meta interface{}) diag.Diagnostics {
	streamingClient := meta.(astraClients).astraStreamingClient.(*astrastreaming.ClientWithResponses)
	client := meta.(astraClients).astraClient.(*astra.ClientWithResponses)
	streamingClientv3 := meta.(astraClients).astraStreamingClientv3

	token := meta.(astraClients).token

	id := resourceData.Id()

	databaseId, keyspace, table, tenantName, err := parseCDCID(id)
	if err != nil {
		return diag.FromErr(err)
	}

	orgBody, _ := client.GetCurrentOrganization(ctx)

	var org OrgId
	bodyBuffer, err := ioutil.ReadAll(orgBody.Body)
	if err != nil {
		return diag.FromErr(err)
	}

	err = json.Unmarshal(bodyBuffer, &org)
	if err != nil {
		fmt.Println("Can't deserialize", orgBody)
	}

	pulsarCluster, pulsarToken, err := prepCDC(ctx, client, databaseId, token, org, err, streamingClient, tenantName)
	if err != nil {
		diag.FromErr(err)
	}

	deleteCDCParams := astrastreaming.DeleteCDCParams{
		XDataStaxPulsarCluster: pulsarCluster,
		Authorization:          pulsarToken,
	}

	deleteRequestBody := astrastreaming.DeleteCDCJSONRequestBody{
		DatabaseId:      databaseId,
		DatabaseName:    resourceData.Get("database_name").(string),
		Keyspace:        keyspace,
		OrgId:           org.ID,
		TableName:       table,
		TopicPartitions: resourceData.Get("topic_partitions").(int),
	}
	getDeleteCDCResponse, err := streamingClientv3.DeleteCDC(ctx, tenantName, &deleteCDCParams, deleteRequestBody)

	if err != nil {
		diag.FromErr(err)
	}
	if !strings.HasPrefix(getDeleteCDCResponse.Status, "2") {
		body, _ := ioutil.ReadAll(getDeleteCDCResponse.Body)
		return diag.Errorf("Error deleting cdc %s", body)
	}

	// Deleted. Remove from state.
	resourceData.SetId("")

	return nil

}

type CDCResult []struct {
	OrgID           string    `json:"orgId"`
	ClusterName     string    `json:"clusterName"`
	Tenant          string    `json:"tenant"`
	Namespace       string    `json:"namespace"`
	ConnectorName   string    `json:"connectorName"`
	ConfigType      string    `json:"configType"`
	DatabaseID      string    `json:"databaseId"`
	DatabaseName    string    `json:"databaseName"`
	Keyspace        string    `json:"keyspace"`
	DatabaseTable   string    `json:"databaseTable"`
	ConnectorStatus string    `json:"connectorStatus"`
	CdcStatus       string    `json:"cdcStatus"`
	CodStatus       string    `json:"codStatus"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
	EventTopic      string    `json:"eventTopic"`
	DataTopic       string    `json:"dataTopic"`
	Instances       int       `json:"instances"`
	CPU             int       `json:"cpu"`
	Memory          int       `json:"memory"`
}

func resourceCDCRead(ctx context.Context, resourceData *schema.ResourceData, meta interface{}) diag.Diagnostics {
	streamingClient := meta.(astraClients).astraStreamingClient.(*astrastreaming.ClientWithResponses)
	client := meta.(astraClients).astraClient.(*astra.ClientWithResponses)
	streamingClientv3 := meta.(astraClients).astraStreamingClientv3

	token := meta.(astraClients).token

	id := resourceData.Id()

	databaseId, keyspace, table, tenantName, err := parseCDCID(id)
	if err != nil {
		return diag.FromErr(err)
	}

	orgBody, _ := client.GetCurrentOrganization(ctx)

	var org OrgId
	bodyBuffer, err := ioutil.ReadAll(orgBody.Body)
	if err != nil {
		return diag.FromErr(err)
	}

	err = json.Unmarshal(bodyBuffer, &org)
	if err != nil {
		fmt.Println("Can't deserialize", orgBody)
	}

	pulsarCluster, pulsarToken, err := prepCDC(ctx, client, databaseId, token, org, err, streamingClient, tenantName)
	if err != nil {
		diag.FromErr(err)
	}

	getCDCParams := astrastreaming.GetCDCParams{
		XDataStaxPulsarCluster: pulsarCluster,
		Authorization:          pulsarToken,
	}
	getCDCResponse, err := streamingClientv3.GetCDC(ctx, tenantName, &getCDCParams)
	if err != nil {
		diag.FromErr(err)
	}
	if !strings.HasPrefix(getCDCResponse.Status, "2") {
		body, _ := ioutil.ReadAll(getCDCResponse.Body)
		return diag.Errorf("Error getting cdc config %s", body)
	}

	body, _ := ioutil.ReadAll(getCDCResponse.Body)

	var cdcResult CDCResult
	err = json.Unmarshal(body, &cdcResult)
	if err != nil {
		fmt.Println("Can't deserialize", body)
	}

	for i := 0; i < len(cdcResult); i++ {
		if cdcResult[i].Keyspace == keyspace {
			if cdcResult[i].DatabaseTable == table {
				return nil
			}
		}
	}

	if err := resourceData.Set("connector_status", cdcResult[0].ConnectorStatus); err != nil {
		return diag.FromErr(err)
	}
	if err := resourceData.Set("data_topic", cdcResult[0].DataTopic); err != nil {
		return diag.FromErr(err)
	}

	// Not found. Remove from state.
	resourceData.SetId("")

	return nil
}

type ServerlessStreamingAvailableRegionsResult []struct {
	Tier            string `json:"tier"`
	Description     string `json:"description"`
	CloudProvider   string `json:"cloudProvider"`
	Region          string `json:"region"`
	RegionDisplay   string `json:"regionDisplay"`
	RegionContinent string `json:"regionContinent"`
	Cost            struct {
		CostPerMinCents         int `json:"costPerMinCents"`
		CostPerHourCents        int `json:"costPerHourCents"`
		CostPerDayCents         int `json:"costPerDayCents"`
		CostPerMonthCents       int `json:"costPerMonthCents"`
		CostPerMinMRCents       int `json:"costPerMinMRCents"`
		CostPerHourMRCents      int `json:"costPerHourMRCents"`
		CostPerDayMRCents       int `json:"costPerDayMRCents"`
		CostPerMonthMRCents     int `json:"costPerMonthMRCents"`
		CostPerMinParkedCents   int `json:"costPerMinParkedCents"`
		CostPerHourParkedCents  int `json:"costPerHourParkedCents"`
		CostPerDayParkedCents   int `json:"costPerDayParkedCents"`
		CostPerMonthParkedCents int `json:"costPerMonthParkedCents"`
		CostPerNetworkGbCents   int `json:"costPerNetworkGbCents"`
		CostPerWrittenGbCents   int `json:"costPerWrittenGbCents"`
		CostPerReadGbCents      int `json:"costPerReadGbCents"`
	} `json:"cost"`
	DatabaseCountUsed               int `json:"databaseCountUsed"`
	DatabaseCountLimit              int `json:"databaseCountLimit"`
	CapacityUnitsUsed               int `json:"capacityUnitsUsed"`
	CapacityUnitsLimit              int `json:"capacityUnitsLimit"`
	DefaultStoragePerCapacityUnitGb int `json:"defaultStoragePerCapacityUnitGb"`
}

type StreamingTokens []struct {
	Iat     int    `json:"iat"`
	Iss     string `json:"iss"`
	Sub     string `json:"sub"`
	Tokenid string `json:"tokenid"`
}

func resourceCDCCreate(ctx context.Context, resourceData *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(astraClients).astraClient.(*astra.ClientWithResponses)
	streamingClient := meta.(astraClients).astraStreamingClient.(*astrastreaming.ClientWithResponses)
	streamingClientv3 := meta.(astraClients).astraStreamingClientv3

	token := meta.(astraClients).token

	table := resourceData.Get("table").(string)
	keyspace := resourceData.Get("keyspace").(string)
	databaseId := resourceData.Get("database_id").(string)
	databaseName := resourceData.Get("database_name").(string)
	topicPartitions := resourceData.Get("topic_partitions").(int)
	tenantName := resourceData.Get("tenant_name").(string)

	orgBody, _ := client.GetCurrentOrganization(ctx)

	var org OrgId
	bodyBuffer, err := ioutil.ReadAll(orgBody.Body)
	if err != nil {
		return diag.FromErr(err)
	}

	err = json.Unmarshal(bodyBuffer, &org)
	if err != nil {
		fmt.Println("Can't deserialize", orgBody)
	}

	cdcRequestJSON := astrastreaming.EnableCDCJSONRequestBody{
		DatabaseId:      databaseId,
		DatabaseName:    databaseName,
		Keyspace:        keyspace,
		OrgId:           org.ID,
		TableName:       table,
		TopicPartitions: topicPartitions,
	}

	pulsarCluster, pulsarToken, err := prepCDC(ctx, client, databaseId, token, org, err, streamingClient, tenantName)
	if err != nil {
		return diag.FromErr(err)
	}

	enableCDCParams := astrastreaming.EnableCDCParams{
		XDataStaxPulsarCluster: pulsarCluster,
		Authorization:          fmt.Sprintf("Bearer %s", pulsarToken),
	}

	var enableClientResult *http.Response
	retryCount := 0
	for enableClientResult == nil || strings.HasPrefix(enableClientResult.Status, "401") {

		enableClientResult, err = streamingClientv3.EnableCDC(ctx, tenantName, &enableCDCParams, cdcRequestJSON)

		if err != nil {
			return diag.FromErr(err)
		}

		if strings.HasPrefix(enableClientResult.Status, "2") {
			bodyBuffer, err = ioutil.ReadAll(enableClientResult.Body)
			break
		}
		if retryCount > 0 {
			fmt.Printf("failed to set up cdc with token %s for table %s", pulsarToken, table)
			time.Sleep(20 * time.Second)
		}
		if retryCount > 6 {
			return diag.Errorf("Could not enable CDC with token: %s", bodyBuffer)
		}
		retryCount = retryCount + 1

		pulsarCluster, pulsarToken, err = prepCDC(ctx, client, databaseId, token, org, err, streamingClient, tenantName)
		if err != nil {
			return diag.FromErr(err)
		}

		enableCDCParams = astrastreaming.EnableCDCParams{
			XDataStaxPulsarCluster: pulsarCluster,
			Authorization:          fmt.Sprintf("Bearer %s", pulsarToken),
		}

	}

	getCDCParams := astrastreaming.GetCDCParams{
		XDataStaxPulsarCluster: pulsarCluster,
		Authorization:          fmt.Sprintf("Bearer %s", pulsarToken),
	}

	var cdcResult CDCResult
	retryCount = 0
	for cdcResult == nil || len(cdcResult) <= 0 {
		getCDCResponse, err := streamingClientv3.GetCDC(ctx, tenantName, &getCDCParams)
		if err != nil {
			return diag.FromErr(err)
		}

		if !strings.HasPrefix(getCDCResponse.Status, "2") {
			bodyBuffer, err = ioutil.ReadAll(getCDCResponse.Body)
			return diag.Errorf("Error enabling client %s", string(bodyBuffer))
		}
		bodyBuffer, err = ioutil.ReadAll(getCDCResponse.Body)
		json.Unmarshal(bodyBuffer, &cdcResult)

		if retryCount > 0 {
			fmt.Printf("failed to set up cdc with token %s for table %s", pulsarToken, table)
			time.Sleep(20 * time.Second)
		}
		if retryCount > 6 {
			return diag.Errorf("Could not enable CDC with token: %s", bodyBuffer)
		}
	}

	if err := resourceData.Set("connector_status", cdcResult[0].ConnectorStatus); err != nil {
		return diag.FromErr(err)
	}
	if err := resourceData.Set("data_topic", cdcResult[0].DataTopic); err != nil {
		return diag.FromErr(err)
	}

	setCDCData(resourceData, fmt.Sprintf("%s/%s/%s/%s", databaseId, keyspace, table, tenantName))

	// Step 3: create sink https://pulsar.apache.org/sink-rest-api/?version=2.8.0&apiversion=v3#operation/registerSink

	return nil
}

func prepCDC(ctx context.Context, client *astra.ClientWithResponses, databaseId string, token string, org OrgId, err error, streamingClient *astrastreaming.ClientWithResponses, tenantName string) (string, string, error) {
	databaseResourceData := schema.ResourceData{}
	db, err := getDatabase(ctx, &databaseResourceData, client, databaseId)
	if err != nil {
		return "", "", err
	}

	// In most astra APIs there are dashes in region names depending on the cloud provider, this seems not to be the case for streaming
	cloudProvider := string(*db.Info.CloudProvider)
	fmt.Printf("%s", cloudProvider)

	pulsarCluster := GetPulsarCluster(cloudProvider, *db.Info.Region)
	pulsarToken, err := getPulsarToken(ctx, pulsarCluster, token, org, err, streamingClient, tenantName)
	return pulsarCluster, pulsarToken, err
}

func GetPulsarCluster(cloudProvider string, rawRegion string) string {
	// In most astra APIs there are dashes in region names depending on the cloud provider, this seems not to be the case for streaming
	region := strings.ReplaceAll(rawRegion, "-", "")
	return strings.ToLower(fmt.Sprintf("pulsar-%s-%s", cloudProvider, region))
}

func getPulsarToken(ctx context.Context, pulsarCluster string, token string, org OrgId, err error, streamingClient *astrastreaming.ClientWithResponses, tenantName string) (string, error) {

	tenantTokenParams := astrastreaming.IdListTenantTokensParams{
		Authorization:          fmt.Sprintf("Bearer %s", token),
		XDataStaxCurrentOrg:    org.ID,
		XDataStaxPulsarCluster: pulsarCluster,
	}

	pulsarTokenResponse, err := streamingClient.IdListTenantTokensWithResponse(ctx, tenantName, &tenantTokenParams)
	if err != nil {
		fmt.Println("Can't generate token", err)
		diag.Errorf("Can't get pulsar token")
		return "", err
	}

	var streamingTokens StreamingTokens
	err = json.Unmarshal(pulsarTokenResponse.Body, &streamingTokens)
	if err != nil {
		fmt.Println("Can't deserialize", pulsarTokenResponse.Body)
		return "", err
	}

	tokenId := streamingTokens[0].Tokenid
	getTokenByIdParams := astrastreaming.GetTokenByIDParams{
		Authorization:          fmt.Sprintf("Bearer %s", token),
		XDataStaxCurrentOrg:    org.ID,
		XDataStaxPulsarCluster: pulsarCluster,
	}

	getTokenResponse, err := streamingClient.GetTokenByIDWithResponse(ctx, tenantName, tokenId, &getTokenByIdParams)

	if err != nil {
		fmt.Println("Can't get token", err)
		return "", err
	}

	pulsarToken := string(getTokenResponse.Body)
	return pulsarToken, err
}

func setCDCData(d *schema.ResourceData, id string) error {
	d.SetId(id)

	return nil
}

func parseCDCID(id string) (string, string, string, string, error) {
	idParts := strings.Split(strings.ToLower(id), "/")
	if len(idParts) != 4 {
		return "", "", "", "", errors.New("invalid role id format: expected databaseId/keyspace/table/tenantName")
	}
	return idParts[0], idParts[1], idParts[2], idParts[3], nil
}
