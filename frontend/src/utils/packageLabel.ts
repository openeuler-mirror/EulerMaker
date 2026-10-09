import { encodeSpecName } from './specName';

export const PACKAGE_NAME_LABEL = "ebs.io/package-name";

export function packageNameLabelValue(packageName: string): string {
  return encodeSpecName(packageName).slice(0, 63).replace(/[-_.]+$/, "");
}
