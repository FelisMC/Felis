// Reject invalid decoded router parameters before constructing API requests.
// api.ts's urlPath encodes and refuses on its own; this keeps a crafted link from reaching a page at all.

/** Server names are DNS-1123 labels; internal/naming holds the stricter rule a
 *  new server's name must meet, so every existing server fits this one. */
export const SERVER_NAME_PARAM = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

/** User ids are opaque: 32 hex characters or a UUID today. */
export const USER_ID_PARAM = /^[A-Za-z0-9_-]{1,128}$/;
