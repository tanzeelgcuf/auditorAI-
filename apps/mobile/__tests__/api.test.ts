import { ApiError } from "../src/api";

describe("ApiError", () => {
  it("carries the status code and message", () => {
    const err = new ApiError(403, "Forbidden");
    expect(err.status).toBe(403);
    expect(err.message).toBe("Forbidden");
    expect(err).toBeInstanceOf(Error);
  });

  it("falls back to a default message", () => {
    const err = new ApiError(500);
    expect(err.status).toBe(500);
    expect(err.message).toContain("500");
  });
});
