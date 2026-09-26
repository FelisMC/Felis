// Compile-time parity between the hand-written wire types (types.ts) and the
// schemas in docs/openapi.yaml, via the generated openapi.gen.ts (`npm run
// gen:api`; CI fails when the generated file is stale). The Go side is held to
// the same schemas by internal/api/openapi_parity_test.go, so a field that
// changes in one place and not the others breaks a build instead of rendering
// `undefined`.
//
// For each pair three things must hold:
//   - the two key sets are equal;
//   - a key the docs mark optional is optional here too (the panel must not
//     count on a field the server may omit);
//   - every documented value fits the panel type (enums included).
// A failure names the offending keys in the error's type.
import type { components } from "./openapi.gen";
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
  Holds<Parity<T.ServerStatus, S["ServerInfo"]>>,
  Holds<Parity<T.MyServerView, S["MyServerView"]>>,
  Holds<Parity<T.AllowlistEntry, S["AllowlistEntry"]>>,
  Holds<Parity<T.FleetServer, S["FleetServer"]>>,
  Holds<Parity<T.BackupView, S["BackupView"]>>,
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
  Holds<Parity<T.SessionView, S["SessionView"]>>,
  Holds<Parity<T.PasskeyCredential, S["PasskeyCredential"]>>,
  Holds<Parity<T.UpdateWindow, S["UpdateWindow"]>>,
  Holds<Parity<T.DBBackupStatus, S["DBBackupStatus"]>>,
  Holds<Parity<T.UpdateReport, S["UpdateReport"]>>,
  Holds<Parity<T.UpdateComponent, S["UpdateComponent"]>>,
];
