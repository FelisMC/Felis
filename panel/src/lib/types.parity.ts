// Check hand-written wire types against generated OpenAPI schemas at compile time.
// Compare key sets, optionality, and documented values, including enums.
import type { components, operations } from "./openapi.gen";
import type * as T from "./types";

type S = components["schemas"];

type OptionalKeys<X> = { [K in keyof X]-?: object extends Pick<X, K> ? K : never }[keyof X];

type Parity<Panel, Docs> = [Exclude<keyof Panel, keyof Docs>, Exclude<keyof Docs, keyof Panel>] extends [
  never,
  never,
]
  ? [Exclude<OptionalKeys<Docs>, OptionalKeys<Panel>>] extends [never]
    ? Docs extends Panel
      ? true
      : { docsValueDoesNotFitPanel: { [K in keyof Docs & keyof Panel]: Docs[K] extends Panel[K] ? never : K }[keyof Docs & keyof Panel] }
    : { optionalInDocsButRequiredInPanel: Exclude<OptionalKeys<Docs>, OptionalKeys<Panel>> }
  : { onlyInPanel: Exclude<keyof Panel, keyof Docs>; onlyInDocs: Exclude<keyof Docs, keyof Panel> };

type Holds<X extends true> = X;

export type WireParity = [
  Holds<Parity<T.ServerPowerResult, operations["wake" | "stop" | "restart" | "kill"]["responses"][202]["content"]["application/json"]>>,
  Holds<Parity<T.ServerStatus, S["ServerInfo"]>>,
  Holds<Parity<T.StartupEnvVar, S["StartupEnvVar"]>>,
  Holds<Parity<T.ServerStartupSettings, S["ServerStartupSettings"]>>,
  Holds<Parity<T.StartupSettingsPatch, S["ServerStartupSettingsPatch"]>>,
  Holds<Parity<T.UsagePoint, S["UsagePoint"]>>,
  Holds<Parity<T.ServerUsage, S["ServerUsage"]>>,
  Holds<Parity<T.ConsoleHistoryPage, S["ConsoleHistoryPage"]>>,
  Holds<Parity<T.ConsoleCommandResult, S["ConsoleCommandResult"]>>,
  Holds<Parity<T.MailSettings, S["MailSettings"]>>,
  Holds<Parity<T.MailSettingsRequest, S["MailSettingsRequest"]>>,
  Holds<Parity<T.ExecutionNode, S["ExecutionNode"]>>,
  Holds<Parity<T.FailoverPolicySettings, S["FailoverPolicySettings"]>>,
  Holds<Parity<T.ArchivePolicySettings, S["ArchivePolicySettings"]>>,
  Holds<Parity<T.HeartbeatSettings, S["HeartbeatSettings"]>>,
  Holds<Parity<T.FailoverBackup, S["FailoverBackup"]>>,
  Holds<Parity<T.FailoverServer, S["FailoverServer"]>>,
  Holds<Parity<T.FailoverPlan, S["FailoverPlan"]>>,
  Holds<Parity<T.FailoverSkip, S["FailoverSkip"]>>,
  Holds<Parity<T.FailoverResult, S["FailoverResult"]>>,
  Holds<Parity<T.NodeControlRequest, S["NodeControlRequest"]>>,
  Holds<Parity<T.NodeControlTask, S["NodeControlTask"]>>,
  Holds<Parity<T.HostOverview, S["HostOverview"]>>,
  Holds<Parity<T.MyServerView, S["MyServerView"]>>,
  Holds<Parity<T.AllowlistEntry, S["AllowlistEntry"]>>,
  Holds<Parity<T.FleetServer, S["FleetServer"]>>,
  Holds<Parity<T.Schedule, S["Schedule"]>>,
  Holds<Parity<T.ScheduleInput, S["ScheduleInput"]>>,
  Holds<Parity<T.BackupView, S["BackupView"]>>,
  Holds<Parity<T.ExportTicket, S["ExportTicket"]>>,
  Holds<Parity<T.ExportStatus, S["ExportStatus"]>>,
  Holds<Parity<T.FileUploadSession, S["FileUploadSession"]>>,
  Holds<Parity<T.FileOp, S["FileOp"]>>,
  Holds<Parity<T.FileOpError, S["FileOpError"]>>,
  Holds<Parity<T.ExtensionMetadata, S["ExtensionMetadata"]>>,
  Holds<Parity<T.ServerExtension, S["ServerExtension"]>>,
  Holds<Parity<T.ServerExtensionsListing, S["ServerExtensionsListing"]>>,
  Holds<Parity<T.PaperGlobalValues, S["PaperGlobalValues"]>>,
  Holds<Parity<T.PaperGlobalConfig, S["PaperGlobalConfig"]>>,
  Holds<Parity<T.PaperGlobalPatch, S["PaperGlobalPatch"]>>,
  Holds<Parity<T.Build, S["Build"]>>,
  Holds<Parity<T.BuildScan, S["BuildScan"]>>,
  Holds<Parity<T.ScanSummary, S["ScanSummary"]>>,
  Holds<Parity<T.ScanPolicy, S["ScanPolicy"]>>,
  Holds<Parity<T.ScanFinding, S["ScanFinding"]>>,
  Holds<Parity<T.WhitelistImage, S["Image"]>>,
  Holds<Parity<T.Submission, S["Submission"]>>,
  Holds<Parity<T.ContextUploadProgress, S["ContextUploadProgress"]>>,
  Holds<Parity<T.UserView, S["UserView"]>>,
  Holds<Parity<T.UserDetail, S["UserDetail"]>>,
  Holds<Parity<T.QuotaView, S["QuotaView"]>>,
  Holds<Parity<T.QuotaLimits, S["QuotaLimits"]>>,
  Holds<Parity<T.ServerQuota, S["ServerQuota"]>>,
  Holds<Parity<T.SelfServiceQuotaSettings, S["SelfServiceQuotaSettings"]>>,
  Holds<Parity<T.ServerSoftware, S["ServerSoftware"]>>,
  Holds<Parity<T.SoftwareRequest, S["SoftwareRequest"]>>,
  Holds<Parity<T.SoftwareVersion, S["SoftwareVersion"]>>,
  Holds<Parity<T.SoftwareBuild, S["SoftwareBuild"]>>,
  Holds<Parity<T.SessionView, S["SessionView"]>>,
  Holds<Parity<T.PasskeyCredential, S["PasskeyCredential"]>>,
  Holds<Parity<T.UpdateWindow, S["UpdateWindow"]>>,
  Holds<Parity<T.DBBackupStatus, S["DBBackupStatus"]>>,
  Holds<Parity<T.UpdateReport, S["UpdateReport"]>>,
  Holds<Parity<T.UpdateComponent, S["UpdateComponent"]>>,
];
