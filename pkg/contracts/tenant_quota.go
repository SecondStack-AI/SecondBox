package contracts

type UpdateTenantQuotaRequest struct {
	AggregateQuota TenantQuota `json:"aggregateQuota"`
}
