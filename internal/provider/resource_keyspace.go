package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/datastax/astra-client-go/v2/astra"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/retry"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
)

// Mutex for synchronizing Keyspace creation
var keyspaceMutex sync.Mutex

func resourceKeyspace() *schema.Resource {
	return &schema.Resource{
		Description:   "`astra_keyspace` provides a keyspace resource. Keyspaces are groupings of tables for Cassandra. `astra_keyspace` resources are associated with a database id. You can have multiple keyspaces per DB in addition to the default keyspace provided in the `astra_database` resource.",
		CreateContext: resourceKeyspaceCreate,
		ReadContext:   resourceKeyspaceRead,
		DeleteContext: resourceKeyspaceDelete,

		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},

		Schema: map[string]*schema.Schema{
			// Required
			"name": {
				Description:      "Keyspace name can have up to 48 alpha-numeric characters and contain underscores; only letters and numbers are supported as the first character.",
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
		},
	}
}

func resourceKeyspaceCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(astraClients).astraClient.(*astra.ClientWithResponses)

	databaseID := d.Get("database_id").(string)
	keyspaceName := d.Get("name").(string)

	//Wait for DB to be in Active status
	if err := retry.RetryContext(ctx, d.Timeout(schema.TimeoutCreate), func() *retry.RetryError {
		keyspaceMutex.Lock()
		res, err := client.GetDatabaseWithResponse(ctx, astra.DatabaseIdParam(databaseID))
		keyspaceMutex.Unlock()
		// Errors sending request should be retried and are assumed to be transient
		if err != nil {
			return retry.RetryableError(err)
		}

		// Status code >=5xx are assumed to be transient
		if res.StatusCode() >= 500 {
			return retry.RetryableError(fmt.Errorf("error while fetching database: %s", string(res.Body)))
		}

		// Status code > 200 NOT retried
		if res.StatusCode() > 200 || res.JSON200 == nil {
			return retry.NonRetryableError(fmt.Errorf("unexpected response fetching database: %s", string(res.Body)))
		}

		// Success fetching database
		db := res.JSON200
		switch db.Status {
		case astra.ERROR, astra.TERMINATED, astra.TERMINATING:
			// If the database reached a terminal state it will never become active
			return retry.NonRetryableError(fmt.Errorf("database failed to reach active status: status=%s", db.Status))
		case astra.ACTIVE:
			keyspaceMutex.Lock()
			resp, err := client.AddKeyspaceWithResponse(ctx, astra.DatabaseIdParam(databaseID), astra.KeyspaceNameParam(keyspaceName))
			keyspaceMutex.Unlock()
			if err != nil {
				return retry.NonRetryableError(fmt.Errorf("error calling add keyspace (not retrying) %s", err))
			} else if resp.StatusCode() == 409 {
				// DevOps API returns 409 for concurrent modifications, these need to be retried.
				return retry.RetryableError(fmt.Errorf("error adding keyspace to database (retrying): %s", string(resp.Body)))
			} else if resp.StatusCode() == 401 {
				// DevOps API returns 401 Unauthorized for requests without the keyspace create permission
				return retry.NonRetryableError(fmt.Errorf("error adding keyspace to database (insufficient permissions, role missing 'db-keyspace-create')"))
			} else if resp.StatusCode() >= 400 {
				return retry.NonRetryableError(fmt.Errorf("error adding keyspace to database (not retrying): %s", string(resp.Body)))
			}

			if err := setKeyspaceResourceData(d, databaseID, keyspaceName); err != nil {
				return retry.NonRetryableError(fmt.Errorf("error setting keyspace data (not retrying) %s", err))
			}

			return nil
		default:
			return retry.RetryableError(fmt.Errorf("expected database to be active but is %s", db.Status))
		}
	}); err != nil {
		return diag.FromErr(err)
	}

	return nil
}

func resourceKeyspaceRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(astraClients).astraClient.(*astra.ClientWithResponses)

	id := d.Id()
	databaseID, keyspaceName, err := parseKeyspaceID(id)
	if err != nil {
		return diag.FromErr(err)
	}

	keyspaces, err := listKeyspaces(ctx, client, databaseID)
	if err != nil {
		return diag.FromErr(err)
	}

	for _, k := range keyspaces {
		if k == keyspaceName {
			if err := setKeyspaceResourceData(d, databaseID, keyspaceName); err != nil {
				return diag.FromErr(err)
			}
			return nil
		}
	}

	// Keyspace not found. Remove from state.
	d.SetId("")

	return nil
}

func resourceKeyspaceDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(astraClients).astraClient.(*astra.ClientWithResponses)

	databaseID := d.Get("database_id").(string)
	keyspaceName := d.Get("name").(string)

	//Wait for DB to be in Active status
	if err := retry.RetryContext(ctx, d.Timeout(schema.TimeoutCreate), func() *retry.RetryError {
		keyspaceMutex.Lock()
		res, err := client.GetDatabaseWithResponse(ctx, astra.DatabaseIdParam(databaseID))
		keyspaceMutex.Unlock()
		// Errors sending request should be retried and are assumed to be transient
		if err != nil {
			return retry.RetryableError(err)
		}

		// Status code >=5xx are assumed to be transient
		if res.StatusCode() >= 500 {
			return retry.RetryableError(fmt.Errorf("error while fetching database: %s", string(res.Body)))
		}

		// Status code > 200 NOT retried
		if res.StatusCode() > 200 || res.JSON200 == nil {
			return retry.NonRetryableError(fmt.Errorf("unexpected response fetching database: %s", string(res.Body)))
		}

		// Success fetching database
		db := res.JSON200
		switch db.Status {
		case astra.ERROR, astra.TERMINATED, astra.TERMINATING:
			// If the database reached a terminal state it will never become active
			return retry.NonRetryableError(fmt.Errorf("database failed to reach active status: status=%s", db.Status))
		case astra.ACTIVE:
			keyspaceMutex.Lock()
			resp, err := client.DropKeyspaceWithResponse(ctx, astra.DatabaseIdParam(databaseID), astra.KeyspaceNameParam(keyspaceName))
			keyspaceMutex.Unlock()
			if err != nil {
				return retry.NonRetryableError(fmt.Errorf("error calling drop keyspace (not retrying) %s", err))
			} else if resp.StatusCode() == 409 {
				// DevOps API returns 409 for concurrent modifications, these need to be retried.
				return retry.RetryableError(fmt.Errorf("error dropping keyspace from database (retrying): %s", string(resp.Body)))
			} else if resp.StatusCode() == 401 {
				// DevOps API returns 401 Unauthorized for requests without the keyspace drop permission
				return retry.NonRetryableError(fmt.Errorf("error adding keyspace to database (insufficient permissions, role missing 'db-keyspace-drop')"))
			} else if resp.StatusCode() >= 400 {
				return retry.NonRetryableError(fmt.Errorf("error dropping keyspace from database (not retrying): %s", string(resp.Body)))
			}

			d.SetId("")
			return nil
		default:
			return retry.RetryableError(fmt.Errorf("expected database to be active but is %s", db.Status))
		}
	}); err != nil {
		return diag.FromErr(err)
	}
	d.SetId("")
	return nil
}

func setKeyspaceResourceData(d *schema.ResourceData, databaseID string, keyspaceName string) error {
	d.SetId(fmt.Sprintf("%s/keyspace/%s", databaseID, keyspaceName))
	if err := d.Set("name", keyspaceName); err != nil {
		return err
	}
	if err := d.Set("database_id", databaseID); err != nil {
		return err
	}

	return nil
}

func parseKeyspaceID(id string) (string, string, error) {
	idParts := strings.Split(id, "/keyspace/")
	if len(idParts) != 2 {
		return "", "", errors.New("invalid keyspace id format: expected database_id/keyspace")
	}
	return idParts[0], idParts[1], nil
}
