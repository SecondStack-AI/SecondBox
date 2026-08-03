import type { Route } from "./transport.generated.ts";

export interface TransportRequestOptions {
  readonly pathParameters?: Readonly<Record<string, string>>;
  readonly queryParameters?: Readonly<Record<string, string>>;
  readonly headers?: Readonly<Record<string, string>>;
  readonly body?: BodyInit;
  readonly contentType?: string;
  readonly signal?: AbortSignal;
}

export class SecondBoxAPIError extends Error {
  public readonly response: Response;

  public constructor(response: Response) {
    super(`SecondBox API request failed: status=${String(response.status)}`);
    this.name = "SecondBoxAPIError";
    this.response = response;
  }
}

export class SecondBoxClient {
  readonly #baseURL: URL;
  readonly #token: string;
  readonly #fetch: typeof fetch;
  readonly #tenantRef: string;
  readonly #subjectRef: string;

  public constructor(
    rawURL: string,
    token: string,
    fetcher: typeof fetch,
    tenantRef = "secondbox",
    subjectRef = "secondbox-admin",
  ) {
    const baseURL = new URL(rawURL);
    if (!["http:", "https:"].includes(baseURL.protocol) || baseURL.search !== "" || baseURL.hash !== "") {
      throw new Error("SecondBox client URL must be an absolute HTTP endpoint without query or fragment");
    }
    if (token === "" || tenantRef === "" || subjectRef === "") {
      throw new Error("SecondBox client token, tenant reference, and subject reference are required");
    }
    this.#baseURL = baseURL;
    this.#token = token;
    this.#fetch = fetcher;
    this.#tenantRef = tenantRef;
    this.#subjectRef = subjectRef;
  }

  public async send(route: Route, options: TransportRequestOptions = {}): Promise<Response> {
    let path = route.path;
    for (const match of path.matchAll(/\{([^}]+)\}/g)) {
      const name = match[1];
      if (name === undefined) throw new Error("SecondBox client path template is malformed");
      const value = options.pathParameters?.[name];
      if (value === undefined || value === "") {
        throw new Error(`SecondBox client missing required path parameter ${name}`);
      }
      path = path.replace(`{${name}}`, encodeURIComponent(value));
    }
    const endpoint = new URL(path, this.#baseURL);
    for (const [name, value] of Object.entries(options.queryParameters ?? {})) {
      endpoint.searchParams.append(name, value);
    }
    const headers = new Headers(options.headers);
    headers.set("Authorization", `Bearer ${this.#token}`);
    headers.set("X-SecondBox-Tenant-Ref", this.#tenantRef);
    headers.set("X-SecondBox-Subject-Ref", this.#subjectRef);
    const contentType = options.contentType ?? (options.body === undefined ? undefined : route.contentType);
    if (contentType !== undefined) headers.set("Content-Type", contentType);
    const response = await this.#fetch(endpoint, {
      method: route.method,
      headers,
      ...(options.body === undefined ? {} : { body: options.body }),
      ...(options.signal === undefined ? {} : { signal: options.signal }),
    });
    if (!response.ok) throw new SecondBoxAPIError(response);
    return response;
  }
}

export function encodeJSONBody(value: unknown): string {
  return JSON.stringify(value);
}
