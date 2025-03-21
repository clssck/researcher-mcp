declare module 'axios-retry' {
  import { AxiosInstance, AxiosError } from 'axios';

  export interface AxiosRetryOptions {
    retries?: number;
    retryDelay?: (retryCount: number, error: AxiosError) => number;
    retryCondition?: (error: AxiosError) => boolean;
    onRetry?: (retryCount: number, error: AxiosError) => void;
  }

  export const exponentialDelay: (retryCount: number) => number;
  export const isNetworkError: (error: AxiosError) => boolean;
  export const isRetryableError: (error: AxiosError) => boolean;
  export const isSafeRequestError: (error: AxiosError) => boolean;
  export const isIdempotentRequestError: (error: AxiosError) => boolean;

  export default function axiosRetry(
    axios: AxiosInstance,
    options: AxiosRetryOptions,
  ): void;
}
