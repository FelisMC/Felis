import { clientError } from "@/lib/api";
import { base64urlToBytes, bytesToBase64url } from "@/lib/utils";

/** requestAssertion runs the WebAuthn get() ceremony for the server's request
 *  options (their JSON form: challenge and credential ids base64url) and returns
 *  the assertion in the JSON form the API's finish endpoints decode. Begins that
 *  answer with go-webauthn's {"publicKey": {...}} envelope pass the inner object. */
export async function requestAssertion(options: any) {
  const publicKey: PublicKeyCredentialRequestOptions = {
    ...options,
    challenge: base64urlToBytes(options.challenge),
    allowCredentials: options.allowCredentials?.map((cred: any) => ({
      ...cred,
      id: base64urlToBytes(cred.id),
    })),
  };
  const credential = (await navigator.credentials.get({ publicKey })) as PublicKeyCredential | null;
  if (!credential) throw clientError("passkey_no_credential");
  const response = credential.response as AuthenticatorAssertionResponse;
  return {
    id: credential.id,
    rawId: bytesToBase64url(credential.rawId),
    type: credential.type,
    response: {
      clientDataJSON: bytesToBase64url(response.clientDataJSON),
      authenticatorData: bytesToBase64url(response.authenticatorData),
      signature: bytesToBase64url(response.signature),
      userHandle: response.userHandle ? bytesToBase64url(response.userHandle) : null,
    },
  };
}
