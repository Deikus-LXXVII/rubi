// Fails `npm test` early, and clearly, on a Node older than the one the runner needs (see "engines").
const major = Number(process.versions.node.split(".")[0]);
if (!(major >= 22)) {
  console.error(`runner tests need Node >= 22, found v${process.versions.node}`);
  process.exit(1);
}
