package identity

// Identity kinds (identity.identities.kind).
const (
	KindUser           = "user"
	KindGroup          = "group"
	KindServiceAccount = "service_account"
	KindWorkflow       = "workflow"
)

// Identity sources (identity.identities.source).
const (
	SourceLocal        = "local"         // bootstrap, access-request, or workflow-created
	SourceExternal     = "external"      // historical IdentityMapping rows (retired, HOR-582)
	SourceExternalAuto = "external-auto" // reserved for future linked identities
)
