import { beforeEach, describe, expect, it } from "vitest";
import { queryClient } from "~/services/queryClient";
import { LLM_CONFIG_QUERY_KEY } from "~/services/queryKeys";
import { resetLlmConfigStateForNewUser } from "./resetLlmConfigState";

const statusFor = (userId: number) => ({
  queryKey: [LLM_CONFIG_QUERY_KEY, userId],
  queryFn: async () => ({ configured: true, userId }),
  staleTime: Infinity,
});

describe("resetLlmConfigStateForNewUser", () => {
  beforeEach(() => {
    queryClient.clear();
  });

  it("drops every other identity's cached configuration", async () => {
    await queryClient.fetchQuery(statusFor(1));
    await queryClient.fetchQuery(statusFor(2));

    resetLlmConfigStateForNewUser(2);

    expect(queryClient.getQueryData([LLM_CONFIG_QUERY_KEY, 1])).toBeUndefined();
    expect(queryClient.getQueryData([LLM_CONFIG_QUERY_KEY, 2])).toEqual({
      configured: true,
      userId: 2,
    });
  });

  it("drops the previous identity's entry when the identity becomes null", async () => {
    await queryClient.fetchQuery(statusFor(1));
    await queryClient.fetchQuery({
      ...statusFor(1),
      queryKey: [LLM_CONFIG_QUERY_KEY, null],
    });

    resetLlmConfigStateForNewUser(null);

    expect(
      queryClient.getQueryData([LLM_CONFIG_QUERY_KEY, 1]),
    ).toBeUndefined();
    expect(
      queryClient.getQueryData([LLM_CONFIG_QUERY_KEY, null]),
    ).toBeDefined();
  });

  it("leaves unrelated cache entries untouched", async () => {
    queryClient.setQueryData(["feeds", 1], [{ id: 1 }]);
    await queryClient.fetchQuery(statusFor(1));

    resetLlmConfigStateForNewUser(2);

    expect(queryClient.getQueryData(["feeds", 1])).toEqual([{ id: 1 }]);
  });
});
