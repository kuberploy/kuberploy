const registryHost =
  String.raw`(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)*` +
  String.raw`[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?`;
const repositoryComponent = String.raw`[a-z0-9]+(?:(?:\.|_{1,2}|-+)[a-z0-9]+)*`;
const repository = `${repositoryComponent}(?:/${repositoryComponent})*`;
const registryAndRepository = `${registryHost}(?::[1-9][0-9]{0,4})?/${repository}`;

const immutablePattern = new RegExp(
  `^${registryAndRepository}@sha256:[a-f0-9]{64}$`,
);
const tagPattern = new RegExp(
  `^${registryAndRepository}:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`,
);

function hasValidPort(reference: string) {
  const server = reference.slice(0, reference.indexOf("/"));
  const separator = server.lastIndexOf(":");
  if (separator < 0) return true;
  const port = Number(server.slice(separator + 1));
  return Number.isSafeInteger(port) && port >= 1 && port <= 65_535;
}

export function isCanonicalImmutableImage(reference: string) {
  const digestSeparator = reference.lastIndexOf("@");
  return (
    reference.length <= 456 &&
    digestSeparator > 0 &&
    reference.slice(0, digestSeparator).length <= 383 &&
    immutablePattern.test(reference) &&
    hasValidPort(reference)
  );
}

export function isCanonicalTaggedImage(reference: string) {
  return (
    reference.length <= 512 &&
    tagPattern.test(reference) &&
    hasValidPort(reference)
  );
}
